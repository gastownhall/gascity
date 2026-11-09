package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// sweepIOSpyStore records every read and write the orphan-release sweep sends to
// a work store, so a test can assert a skipped bead cost the store nothing.
//
// It embeds the *MemStore itself, not the beads.Store interface, so the store's
// optional capabilities (the fenced conditional release) stay reachable and a
// release that is refused is refused for a reason the test names.
type sweepIOSpyStore struct {
	*beads.MemStore
	mu      sync.Mutex
	lists   []beads.ListQuery
	gets    []string
	updates []string
}

func (s *sweepIOSpyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.lists = append(s.lists, q)
	s.mu.Unlock()
	return s.MemStore.List(q)
}

func (s *sweepIOSpyStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	s.gets = append(s.gets, id)
	s.mu.Unlock()
	return s.MemStore.Get(id)
}

func (s *sweepIOSpyStore) Update(id string, opts beads.UpdateOpts) error {
	s.mu.Lock()
	s.updates = append(s.updates, id)
	s.mu.Unlock()
	return s.MemStore.Update(id, opts)
}

// liveWorkRefusingStore answers every live work-assignment verification with an
// empty list, as a store whose row no longer matches would. Session listings
// (Label set) pass through untouched.
type liveWorkRefusingStore struct {
	*beads.MemStore
}

func (s liveWorkRefusingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live && q.Status != "" && q.Label == "" {
		return nil, nil
	}
	return s.MemStore.List(q)
}

// seedSweepOrphan creates a pool-routed work bead held by a dead assignee and
// returns the tick-snapshot copy of it with the deferral shape the caller asks
// for. The store copy keeps what Create stored; the snapshot copy is the one the
// sweep reads its gates from.
func seedSweepOrphan(t *testing.T, mem *beads.MemStore, status string, deferUntil *time.Time, indefinite bool, extraMeta map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{"gc.routed_to": "worker"}
	for k, v := range extraMeta {
		meta[k] = v
	}
	b, err := mem.Create(beads.Bead{Title: "orphaned pool work", Assignee: "worker-dead", Metadata: meta})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if status != "open" {
		if err := mem.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Set work status: %v", err)
		}
	}
	snap, err := mem.Get(b.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}
	snap.DeferUntil = deferUntil
	snap.IndefinitelyDeferred = indefinite
	return snap
}

type sweepResult struct {
	released []releasedPoolAssignment
	attrs    map[string]any
}

// runSweep runs one orphan-release sweep over work, all owned by store.
func runSweep(store beads.Store, work ...beads.Bead) sweepResult {
	var res sweepResult
	stores := make([]beads.Store, len(work))
	refs := make([]string, len(work))
	for i := range work {
		stores[i] = store
	}
	res.released = releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		testPoolReleaseConfig(),
		"",
		nil,
		work,
		stores,
		refs,
		nil,
		nil,
		func(_ TraceSiteCode, name string, _ time.Time, attrs map[string]any) {
			if name == "bead_reconcile.release_orphaned_pool_assignments.release_sweep" {
				res.attrs = attrs
			}
		},
		nil,
	)
	return res
}

// TestReleaseOrphanedPoolAssignments_SkipsDeferredOpenWorkWithoutAnyRead pins the
// per-tick cost fix (ga-y0ft4d): open work that bd hides as deferred is not a
// release candidate, and skipping it spends no store I/O at all. Both deferral
// shapes BdStore reports are covered: a dated deferral and a status-based
// deferral with no date.
func TestReleaseOrphanedPoolAssignments_SkipsDeferredOpenWorkWithoutAnyRead(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	tests := []struct {
		name       string
		deferUntil *time.Time
		indefinite bool
	}{
		{"timed deferral", &future, false},
		{"indefinite deferral", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			spy := &sweepIOSpyStore{MemStore: mem}
			work := seedSweepOrphan(t, mem, "open", tt.deferUntil, tt.indefinite, nil)

			res := runSweep(spy, work)

			if len(res.released) != 0 {
				t.Fatalf("released = %v, want none for deferred open work", res.released)
			}
			if len(spy.lists) != 0 || len(spy.gets) != 0 || len(spy.updates) != 0 {
				t.Fatalf("store I/O for a skipped bead: lists=%#v gets=%v updates=%v, want none", spy.lists, spy.gets, spy.updates)
			}
			got, err := mem.Get(work.ID)
			if err != nil {
				t.Fatalf("Get work bead: %v", err)
			}
			if got.Status != "open" || got.Assignee != "worker-dead" {
				t.Fatalf("bead after sweep: status=%q assignee=%q, want open/worker-dead unchanged", got.Status, got.Assignee)
			}
			if res.attrs == nil {
				t.Fatal("sweep recorded no release_sweep phase")
			}
			if res.attrs["memoized_count"] != 0 || res.attrs["fallback_count"] != 0 {
				t.Fatalf("phase attrs = %#v, want memoized_count and fallback_count 0 (no liveness probe for a skipped bead)", res.attrs)
			}
		})
	}
}

// TestReleaseOrphanedPoolAssignments_ReleasesInProgressDespiteStaleDeferUntil
// guards the skip's status scope: a claimed (in_progress) bead that still carries
// a leftover future defer_until is live work, not parked work, and is released.
func TestReleaseOrphanedPoolAssignments_ReleasesInProgressDespiteStaleDeferUntil(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	mem := beads.NewMemStore()
	work := seedSweepOrphan(t, mem, "in_progress", &future, false, nil)

	res := runSweep(mem, work)

	if len(res.released) != 1 || res.released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", res.released, work.ID)
	}
	got, err := mem.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("bead after sweep: status=%q assignee=%q, want open/unassigned", got.Status, got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignments_LapsedDeferralIsNotSkipped guards the skip's
// clock: the instant is the sweep's own real time, so a deferral already in the
// past no longer hides the bead, and a plain open orphan is still released.
func TestReleaseOrphanedPoolAssignments_LapsedDeferralIsNotSkipped(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	tests := []struct {
		name       string
		deferUntil *time.Time
	}{
		{"deferral lapsed an hour ago", &past},
		{"never deferred", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			work := seedSweepOrphan(t, mem, "open", tt.deferUntil, false, nil)

			res := runSweep(mem, work)

			if len(res.released) != 1 || res.released[0].ID != work.ID {
				t.Fatalf("released = %v, want [%s]", res.released, work.ID)
			}
			got, err := mem.Get(work.ID)
			if err != nil {
				t.Fatalf("Get work bead: %v", err)
			}
			if got.Status != "open" || got.Assignee != "" {
				t.Fatalf("bead after sweep: status=%q assignee=%q, want open/unassigned", got.Status, got.Assignee)
			}
		})
	}
}

// runDetachedSweep runs one sweep over a detached-probe orphan whose tmux
// session is dead, and reports whether the tmux probe ran at all. The fake tmux
// writes its argv to a file only when it is invoked.
func runDetachedSweep(t *testing.T, status string, deferUntil *time.Time, indefinite bool, wrap func(*beads.MemStore) beads.Store) (sweepResult, bool, string) {
	t.Helper()
	argsPath := filepath.Join(t.TempDir(), "tmux-args")
	installFakeTmuxServer(t, "gascity", fakeTmuxListSessions())
	t.Setenv("FAKE_TMUX_ARGS", argsPath)
	var logs bytes.Buffer
	restore := captureLogOutput(&logs)
	defer restore()

	mem := beads.NewMemStore()
	work := seedSweepOrphan(t, mem, status, deferUntil, indefinite, map[string]string{detachedProbeMetadataKey: testDetachedPoolProbeSpec})
	var store beads.Store = mem
	if wrap != nil {
		store = wrap(mem)
	}

	res := runSweep(store, work)

	_, err := os.Stat(argsPath)
	return res, err == nil, logs.String()
}

// TestReleaseOrphanedPoolAssignments_DetachedProbeNotRunForSkippedOrRefusedWork
// pins the probe ordering: the detached tmux probe is the most expensive gate, so
// it runs only for a bead that is a release candidate AND whose live verification
// passed. A deferred open orphan never reaches it, and neither does one the live
// check refuses. The first row is the positive control proving the fake observes
// a probe.
func TestReleaseOrphanedPoolAssignments_DetachedProbeNotRunForSkippedOrRefusedWork(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	tests := []struct {
		name         string
		status       string
		deferUntil   *time.Time
		indefinite   bool
		wrap         func(*beads.MemStore) beads.Store
		wantProbe    bool
		wantReleased int
	}{
		{name: "control: open orphan is probed and released", status: "open", wantProbe: true, wantReleased: 1},
		{name: "control: in_progress orphan is probed and released", status: "in_progress", wantProbe: true, wantReleased: 1},
		{name: "timed-deferred open orphan is not probed", status: "open", deferUntil: &future},
		{name: "indefinitely-deferred open orphan is not probed", status: "open", indefinite: true},
		{
			name: "orphan whose live verification refuses is not probed", status: "in_progress",
			wrap: func(s *beads.MemStore) beads.Store { return liveWorkRefusingStore{MemStore: s} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, probed, logs := runDetachedSweep(t, tt.status, tt.deferUntil, tt.indefinite, tt.wrap)

			if probed != tt.wantProbe {
				t.Fatalf("tmux probe ran = %v, want %v; logs: %s", probed, tt.wantProbe, logs)
			}
			if len(res.released) != tt.wantReleased {
				t.Fatalf("released = %v, want %d; logs: %s", res.released, tt.wantReleased, logs)
			}
			if !tt.wantProbe && bytes.Contains([]byte(logs), []byte("detached work")) {
				t.Fatalf("logs mention a detached-probe outcome for a bead the probe must not see: %s", logs)
			}
		})
	}
}

// TestReleaseConfirmedOrphanSessionWork_DeferredOpenWorkStillGoesThroughTheLiveCheck
// pins the skip as sweep-only. The orphan-close tie-break (ga-jrnou) must still
// release a deferred open claim held by the confirmed-dead seat, or the seat-close
// guard (which counts deferred work as seat work on NativeDoltStore) stays a
// permanent block. The claim therefore reaches the live verification, and that
// verification is scoped to the seat's identifier.
func TestReleaseConfirmedOrphanSessionWork_DeferredOpenWorkStillGoesThroughTheLiveCheck(t *testing.T) {
	cfg := bindingOrphanConfig(t)
	work := bindingOrphanClaim()
	work.Status = "open"
	future := time.Now().Add(24 * time.Hour)
	work.DeferUntil = &future
	binding := beads.NewMemStoreFrom(1, []beads.Bead{work}, nil)
	spy := &sweepIOSpyStore{MemStore: binding}

	released := releaseConfirmedOrphanSessionWork(
		cfg, spy, map[string]beads.Store{"beads": beads.NewMemStore()}, []beads.Bead{work}, []beads.Store{spy}, bindingOrphanSessionInfo(),
	)

	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %#v, want the deferred open claim %q released", released, work.ID)
	}
	want := beads.ListQuery{Assignee: bindingOrphanSeat, Status: "open", Live: true, TierMode: beads.TierBoth}
	found := false
	for _, q := range spy.lists {
		if q.Live && q.Status == "open" && q.Assignee == bindingOrphanSeat {
			found = true
		}
	}
	if !found {
		t.Fatalf("no scoped live verification List; want one equal to %#v, saw %#v", want, spy.lists)
	}
}
