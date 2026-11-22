package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// A due key that no open session lists is counted orphaned once per key per
// pass, however many items share it, and logged with its resolution; a padded
// key the claim's trimmed match gives to an open session is not an orphan, and
// not-matched still counts only the open session with nothing it matches.
func TestDispatchAllQueuedNudgesCountsOrphanedKeys(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_DEBUG", "1")

	dir := t.TempDir()
	due := time.Now().Add(-time.Minute)
	for _, item := range []queuedNudge{
		newQueuedNudge("gc-gone", "first", due),
		newQueuedNudge("gc-gone", "second", due),
		newQueuedNudge("gc-gone-2", "third", due),
		newQueuedNudge(" helper ", "padded", due),
	} {
		if err := enqueueQueuedNudge(dir, item); err != nil {
			t.Fatalf("enqueueQueuedNudge(%q): %v", item.Agent, err)
		}
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "helper-session",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "helper-session",
			"agent_name":   "helper",
			"template":     "helper",
		},
	}})

	var debugOut strings.Builder
	if _, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, &debugOut); err != nil {
		t.Fatalf("dispatch pass 1: %v", err)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := state.DispatchSkips["orphaned"]; got != 2 {
		t.Fatalf("pass 1: DispatchSkips[orphaned] = %d, want 2 (gc-gone, gc-gone-2); full map %#v", got, state.DispatchSkips)
	}
	if got := state.DispatchSkips["not-matched"]; got != 1 {
		t.Fatalf("pass 1: DispatchSkips[not-matched] = %d, want 1 (the helper session's exact match misses the padded key); full map %#v", got, state.DispatchSkips)
	}
	out := debugOut.String()
	for _, want := range []string{"route=skip reason=orphaned", "agent=gc-gone ", "agent=gc-gone-2 ", "resolution=unfenced", "items=2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "reason=orphaned agent= helper") || strings.Contains(out, "reason=orphaned agent=helper") {
		t.Fatalf("the padded helper key was reported orphaned:\n%s", out)
	}

	if _, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, nil); err != nil {
		t.Fatalf("dispatch pass 2: %v", err)
	}
	state, err = nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := state.DispatchSkips["orphaned"]; got != 4 {
		t.Fatalf("pass 2: DispatchSkips[orphaned] = %d, want 4; full map %#v", got, state.DispatchSkips)
	}
	if len(state.Pending) != 4 {
		t.Fatalf("pending = %d, want all 4 items still queued", len(state.Pending))
	}
}

// orphanSeatFixture is a supervisor-mode city on the file store whose
// sessions are created through the production session manager.
type orphanSeatFixture struct {
	dir   string
	store beads.NudgesStore
	fake  *runtime.Fake
	mgr   *session.Manager
}

func newOrphanSeatFixture(t *testing.T) *orphanSeatFixture {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	f := &orphanSeatFixture{dir: t.TempDir(), fake: runtime.NewFake()}
	f.store = openNudgeBeadStore(f.dir)
	f.mgr = newSessionManagerWithConfig(f.dir, f.store.Store, f.fake, nil)
	f.fake.Activity = map[string]time.Time{}
	return f
}

// createSeat creates a worker session holding a pool seat: pool-managed with
// agentName and slot. An empty slot creates a manual (non-pool) session.
func (f *orphanSeatFixture) createSeat(t *testing.T, agentName, slot string) session.Info {
	t.Helper()
	meta := map[string]string{"session_origin": "manual", "agent_name": agentName}
	if slot != "" {
		meta["session_origin"] = "ephemeral"
		meta["pool_managed"] = "true"
		meta["pool_slot"] = slot
	}
	info, err := f.mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "worker", Command: "codex", WorkDir: f.dir, Provider: "codex", Hints: runtime.Config{WorkDir: f.dir}, ExtraMeta: meta})
	if err != nil {
		t.Fatalf("CreateSession %s: %v", agentName, err)
	}
	return f.get(t, info.ID)
}

// startIdle starts id's runtime and marks it idle.
func (f *orphanSeatFixture) startIdle(t *testing.T, id string) session.Info {
	t.Helper()
	if err := f.mgr.Start(context.Background(), testOperatorIn(t, f.dir), id, "", runtime.Config{WorkDir: f.dir}); err != nil {
		t.Fatalf("Start %s: %v", id, err)
	}
	info := f.get(t, id)
	f.fake.Activity[info.SessionName] = time.Now().Add(-10 * time.Second)
	return info
}

func (f *orphanSeatFixture) get(t *testing.T, id string) session.Info {
	t.Helper()
	info, err := sessionFrontDoor(f.store.Store).Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return info
}

// closeSession closes id through the production close path.
func (f *orphanSeatFixture) closeSession(t *testing.T, id string) {
	t.Helper()
	if err := f.mgr.Close(context.Background(), testOperatorIn(t, f.dir), id); err != nil {
		t.Fatalf("Close %s: %v", id, err)
	}
}

// enqueueFencedTo queues what the hook-claim path writes for an unaliased
// pool worker: an item keyed by the session ID and fenced to it.
func (f *orphanSeatFixture) enqueueFencedTo(t *testing.T, p session.Info, message string, createdAt time.Time) queuedNudge {
	t.Helper()
	item := newQueuedNudgeWithOptions(p.ID, message, "hook-claim-continuation", createdAt, queuedNudgeOptions{SessionID: p.ID, ContinuationEpoch: p.ContinuationEpoch})
	if err := enqueueQueuedNudge(f.dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	return item
}

func (f *orphanSeatFixture) dispatch(t *testing.T, sp runtime.Provider, debugOut *strings.Builder) int {
	t.Helper()
	snapshot, err := loadSessionBeadSnapshot(f.store.Store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	var w io.Writer
	if debugOut != nil {
		w = debugOut
	}
	delivered, err := dispatchAllQueuedNudges(f.dir, supervisorCfg(), f.store.Store, f.store.Store, sp, snapshot, w)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	return delivered
}

func (f *orphanSeatFixture) nudgeCalls() []string {
	var msgs []string
	for _, call := range f.fake.Calls {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			msgs = append(msgs, call.Name+": "+call.Message)
		}
	}
	return msgs
}

func (f *orphanSeatFixture) state(t *testing.T) nudgequeue.State {
	t.Helper()
	state, err := nudgequeue.LoadState(f.dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return state
}

// A nudge keyed by and fenced to a closed, unaliased pool session is
// delivered once to the one running session holding the same pool seat,
// re-fenced onto it, and acked; the key is not counted orphaned.
func TestDispatchAdoptsOrphanedPoolSeatNudgeForSuccessor(t *testing.T) {
	f := newOrphanSeatFixture(t)
	p := f.createSeat(t, "worker-2", "2")
	p = f.startIdle(t, p.ID)
	item := f.enqueueFencedTo(t, p, "continue the claimed work", time.Now().Add(-time.Minute))
	f.closeSession(t, p.ID)
	closed := f.get(t, p.ID)
	if !closed.Closed || closed.AgentName != "worker-2" || closed.Template != "worker" || closed.PoolSlot != "2" || !isPoolManagedSessionInfo(closed) {
		t.Fatalf("setup: closed predecessor row = %+v, want it closed and keeping its pool seat", closed)
	}
	s := f.createSeat(t, "worker-2", "2")
	s = f.startIdle(t, s.ID)
	f.fake.Calls = nil

	var refenced []queuedNudge
	sp := onNudgeProvider{Fake: f.fake, onNudge: func(string) {
		refenced = append(refenced, f.state(t).InFlight...)
	}}
	delivered := f.dispatch(t, sp, nil)

	msgs := f.nudgeCalls()
	if delivered != 1 || len(msgs) != 1 || !strings.HasPrefix(msgs[0], s.SessionName+":") || !strings.Contains(msgs[0], "continue the claimed work") {
		t.Fatalf("delivered=%d nudges=%v, want the predecessor's nudge delivered once to successor %s", delivered, msgs, s.SessionName)
	}
	if len(refenced) != 1 || refenced[0].ID != item.ID || refenced[0].SessionID != s.ID || refenced[0].ContinuationEpoch != s.ContinuationEpoch {
		t.Fatalf("in flight at delivery = %+v, want %s re-fenced to %s epoch %q", refenced, item.ID, s.ID, s.ContinuationEpoch)
	}
	state := f.state(t)
	for _, bucket := range [][]queuedNudge{state.Pending, state.InFlight, state.Dead} {
		for _, left := range bucket {
			if left.Agent == p.ID {
				t.Fatalf("item %s still queued under the predecessor's key: %+v", left.ID, left)
			}
		}
	}
	if got := state.DispatchSkips["orphaned"]; got != 0 {
		t.Fatalf("DispatchSkips[orphaned] = %d, want 0 for an adopted key; full map %#v", got, state.DispatchSkips)
	}
}

// An orphaned nudge whose fenced predecessor's seat has no unique holder is
// not delivered: it stays pending, is counted orphaned, and only the TTL sweep
// dead-letters it.
func TestDispatchDoesNotAdoptOrphanWithoutUniqueSeatHolder(t *testing.T) {
	cases := []struct {
		name string
		// predSlot is the predecessor's pool slot; empty makes it manual.
		predSlot string
		// holders are the slots of the running worker-2 sessions created after
		// the predecessor closes.
		holders []string
		// archiveHolder archives the one holder (open, no longer holding its fence).
		archiveHolder bool
		unfenced      bool
		resolution    string
	}{
		{name: "vacant seat", predSlot: "2", resolution: "seat-vacant"},
		{name: "two holders", predSlot: "2", holders: []string{"2", "2"}, resolution: "seat-ambiguous"},
		{name: "manual predecessor", predSlot: "", holders: []string{"2"}, resolution: "not-pool-seat"},
		{name: "pool_slot mismatch", predSlot: "2", holders: []string{"3"}, resolution: "seat-vacant"},
		{name: "only candidate archived", predSlot: "2", holders: []string{"2"}, archiveHolder: true, resolution: "seat-vacant"},
		{name: "unfenced item", predSlot: "2", holders: []string{"2"}, unfenced: true, resolution: "unfenced"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrphanSeatFixture(t)
			p := f.startIdle(t, f.createSeat(t, "worker-2", tc.predSlot).ID)
			var item queuedNudge
			if tc.unfenced {
				item = newQueuedNudge(p.ID, "left for the seat", time.Now().Add(-time.Minute))
				if err := enqueueQueuedNudge(f.dir, item); err != nil {
					t.Fatal(err)
				}
			} else {
				item = f.enqueueFencedTo(t, p, "left for the seat", time.Now().Add(-time.Minute))
			}
			f.closeSession(t, p.ID)
			// A running sibling on another slot keeps the city from being empty.
			f.startIdle(t, f.createSeat(t, "worker-9", "9").ID)
			for _, slot := range tc.holders {
				h := f.startIdle(t, f.createSeat(t, "worker-2", slot).ID)
				if tc.archiveHolder {
					if err := f.store.SetMetadata(h.ID, "state", string(session.StateArchived)); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.fake.Calls = nil

			// Session setup resets the environment; enable route logging last.
			t.Setenv("GC_DEBUG", "1")
			var debugOut strings.Builder
			delivered := f.dispatch(t, f.fake, &debugOut)
			if msgs := f.nudgeCalls(); delivered != 0 || len(msgs) != 0 {
				t.Fatalf("delivered=%d nudges=%v, want nothing delivered", delivered, msgs)
			}
			state := f.state(t)
			if len(state.Pending) != 1 || state.Pending[0].ID != item.ID || state.Pending[0].SessionID != item.SessionID {
				t.Fatalf("pending = %+v, want %s still pending with its own fence", state.Pending, item.ID)
			}
			if got := state.DispatchSkips["orphaned"]; got != 1 {
				t.Fatalf("DispatchSkips[orphaned] = %d, want 1; full map %#v", got, state.DispatchSkips)
			}
			if want := "resolution=" + tc.resolution + " "; !strings.Contains(debugOut.String(), want) {
				t.Fatalf("debug output missing %q:\n%s", want, debugOut.String())
			}
		})
	}

	t.Run("expired vacant-seat item is dead-lettered by the TTL sweep", func(t *testing.T) {
		f := newOrphanSeatFixture(t)
		p := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
		item := f.enqueueFencedTo(t, p, "stale", time.Now().Add(-25*time.Hour))
		f.closeSession(t, p.ID)
		f.startIdle(t, f.createSeat(t, "worker-9", "9").ID)
		if delivered := f.dispatch(t, f.fake, nil); delivered != 0 {
			t.Fatalf("delivered = %d, want 0", delivered)
		}
		state := f.state(t)
		if len(state.Pending) != 0 || len(state.Dead) != 1 || state.Dead[0].ID != item.ID || state.Dead[0].LastError != "expired" {
			t.Fatalf("pending=%+v dead=%+v, want %s dead-lettered expired", state.Pending, state.Dead, item.ID)
		}
	})
}

// TestResolveNudgeOrphanSeat pins the typed resolution of one orphaned item:
// through its session fence to the predecessor's pool seat, and to the single
// open session holding that seat once the predecessor no longer holds its
// fence. Every resolution other than successor leaves the key queued.
func TestResolveNudgeOrphanSeat(t *testing.T) {
	cases := []struct {
		name string
		// build returns the item to resolve, the lookup budget, and the
		// expected successor session ID ("" for none).
		build          func(t *testing.T, f *orphanSeatFixture) (item queuedNudge, budget int, wantSuccessor string)
		wantResolution nudgeOrphanResolution
	}{
		{
			name: "closed pool predecessor with a unique successor",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				f.closeSession(t, p.ID)
				s := f.createSeat(t, "worker-2", "2")
				return fencedOrphanItem(p), nudgeOrphanPredecessorLookupsPerPass, s.ID
			},
			wantResolution: nudgeOrphanSuccessor,
		},
		{
			name: "closed pool predecessor whose seat is vacant",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				f.closeSession(t, p.ID)
				return fencedOrphanItem(p), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanSeatVacant,
		},
		{
			name: "two open sessions hold the closed predecessor's seat",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				f.closeSession(t, p.ID)
				f.createSeat(t, "worker-2", "2")
				f.createSeat(t, "worker-2", "2")
				return fencedOrphanItem(p), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanSeatAmbiguous,
		},
		{
			name: "the only open holder sits in a different pool slot",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				f.closeSession(t, p.ID)
				f.createSeat(t, "worker-2", "3")
				return fencedOrphanItem(p), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanSeatVacant,
		},
		{
			name: "closed manual predecessor is not a pool seat",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "")
				f.closeSession(t, p.ID)
				f.createSeat(t, "worker-2", "2")
				return fencedOrphanItem(p), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanNotPoolSeat,
		},
		{
			name: "fenced predecessor is still open and holds its fence",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				f.createSeat(t, "worker-2", "2")
				return fencedOrphanItem(p), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanFencedSessionLive,
		},
		{
			name: "no record answers the fenced session id",
			build: func(_ *testing.T, _ *orphanSeatFixture) (queuedNudge, int, string) {
				return fencedOrphanItem(session.Info{ID: "no-such-session"}), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanFencedSessionUnknown,
		},
		{
			name: "unfenced item keyed by the closed predecessor's bead id",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				f.closeSession(t, p.ID)
				f.createSeat(t, "worker-2", "2")
				return newQueuedNudge(p.ID, "continuation", time.Now().Add(-time.Minute)), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanUnfenced,
		},
		{
			name: "unfenced item keyed by no session at all",
			build: func(_ *testing.T, _ *orphanSeatFixture) (queuedNudge, int, string) {
				return newQueuedNudge("ghost-lane", "wisp", time.Now().Add(-time.Minute)), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanUnfenced,
		},
		{
			name: "unfenced item keyed by the open seat holder",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				s := f.createSeat(t, "worker-2", "2")
				return newQueuedNudge(s.ID, "wisp", time.Now().Add(-time.Minute)), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanUnfenced,
		},
		{
			name: "the lookup budget is spent before the store read",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				f.closeSession(t, p.ID)
				f.createSeat(t, "worker-2", "2")
				return fencedOrphanItem(p), 0, ""
			},
			wantResolution: nudgeOrphanLookupDeferred,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrphanSeatFixture(t)
			item, budget, wantSuccessor := tc.build(t, f)
			snapshot, err := loadSessionBeadSnapshot(f.store.Store)
			if err != nil {
				t.Fatalf("loadSessionBeadSnapshot: %v", err)
			}
			seat := newNudgeOrphanSeatResolver(snapshot.OpenInfos(), sessionFrontDoor(f.store.Store).Get, budget).resolve(item)
			if seat.Resolution != tc.wantResolution {
				t.Fatalf("resolution = %q, want %q", seat.Resolution, tc.wantResolution)
			}
			if seat.Successor.ID != wantSuccessor {
				t.Fatalf("successor = %q, want %q", seat.Successor.ID, wantSuccessor)
			}
		})
	}
}

// fencedOrphanItem is a due continuation keyed by and fenced to p, as the
// hook-claim path writes it for an unaliased pool worker.
func fencedOrphanItem(p session.Info) queuedNudge {
	return newQueuedNudgeWithOptions(p.ID, "continuation", "hook-claim-continuation", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: p.ID, ContinuationEpoch: p.ContinuationEpoch})
}

// An adopted orphan still moves only when the claim's own live census lets
// it: a census that still lists the predecessor, or no census at all, leaves
// the item pending under its own fence; a census of the successor alone
// delivers it.
func TestDispatchOrphanAdoptionDefersToClaimCensus(t *testing.T) {
	cases := []struct {
		name string
		// census builds the claim's live census from the predecessor and
		// successor IDs; nil means the census was unavailable.
		census    func(pred, succ string) map[string]struct{}
		delivered bool
	}{
		{name: "census still lists the predecessor", census: func(pred, succ string) map[string]struct{} {
			return map[string]struct{}{pred: {}, succ: {}}
		}},
		{name: "census unavailable", census: func(string, string) map[string]struct{} { return nil }},
		{name: "census lists only the successor", census: func(_, succ string) map[string]struct{} {
			return map[string]struct{}{succ: {}}
		}, delivered: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrphanSeatFixture(t)
			p := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
			item := f.enqueueFencedTo(t, p, "continue the claimed work", time.Now().Add(-time.Minute))
			f.closeSession(t, p.ID)
			s := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
			f.fake.Calls = nil

			census := tc.census(p.ID, s.ID)
			old := liveNudgeFenceSessionIDs
			liveNudgeFenceSessionIDs = func(string) map[string]struct{} { return census }
			t.Cleanup(func() { liveNudgeFenceSessionIDs = old })

			delivered := f.dispatch(t, f.fake, nil)
			msgs := f.nudgeCalls()
			state := f.state(t)
			if tc.delivered {
				if delivered != 1 || len(msgs) != 1 || !strings.HasPrefix(msgs[0], s.SessionName+":") {
					t.Fatalf("delivered=%d nudges=%v, want one delivery to successor %s", delivered, msgs, s.SessionName)
				}
				return
			}
			if delivered != 0 || len(msgs) != 0 {
				t.Fatalf("delivered=%d nudges=%v, want nothing delivered", delivered, msgs)
			}
			if len(state.Pending) != 1 || state.Pending[0].ID != item.ID || state.Pending[0].Agent != p.ID || state.Pending[0].SessionID != p.ID {
				t.Fatalf("pending = %+v, want %s pending under %s with its own fence", state.Pending, item.ID, p.ID)
			}
		})
	}
}

// snapshotCountingQueue counts whole-queue Snapshot reads.
type snapshotCountingQueue struct {
	storebinding.NudgeQueue
	snapshots int
}

func (q *snapshotCountingQueue) Snapshot() (nudgequeue.State, error) {
	q.snapshots++
	return q.NudgeQueue.Snapshot()
}

// getRecordingStore records every single-bead Get.
type getRecordingStore struct {
	beads.Store
	gets []string
}

func (s *getRecordingStore) Get(id string) (beads.Bead, error) {
	s.gets = append(s.gets, id)
	return s.Store.Get(id)
}

// A pass whose every due key an open session lists reads no whole-queue
// snapshot and looks up no session outside the open snapshot (by ID or
// session name); a pass with an
// orphaned key reads the snapshot once and looks up its fenced predecessor.
func TestDispatchSteadyStatePassReadsNoOrphanState(t *testing.T) {
	pass := func(t *testing.T, f *orphanSeatFixture) (delivered int, snapshots int, foreignGets []string) {
		t.Helper()
		snapshot, err := loadSessionBeadSnapshot(f.store.Store)
		if err != nil {
			t.Fatalf("loadSessionBeadSnapshot: %v", err)
		}
		open := make(map[string]bool)
		for _, info := range snapshot.OpenInfos() {
			open[info.ID] = true
			open[info.SessionName] = true
		}
		handle := openNudgeQueue(f.dir, f.store)
		queue := &snapshotCountingQueue{NudgeQueue: handle.Queue}
		handle.Queue = queue
		sessStore := &getRecordingStore{Store: f.store.Store}
		delivered, err = dispatchQueuedNudges(handle, f.dir, supervisorCfg(), f.store.Store, sessStore, f.fake, snapshot, nil)
		if err != nil {
			t.Fatalf("dispatchQueuedNudges: %v", err)
		}
		for _, id := range sessStore.gets {
			if !open[id] {
				foreignGets = append(foreignGets, id)
			}
		}
		return delivered, queue.snapshots, foreignGets
	}

	t.Run("every due key matched", func(t *testing.T) {
		f := newOrphanSeatFixture(t)
		s := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
		f.enqueueFencedTo(t, s, "keep going", time.Now().Add(-time.Minute))
		f.startIdle(t, f.createSeat(t, "worker-9", "9").ID)
		f.fake.Calls = nil

		delivered, snapshots, foreign := pass(t, f)
		if delivered != 1 {
			t.Fatalf("delivered = %d, want 1", delivered)
		}
		if snapshots != 0 || len(foreign) != 0 {
			t.Fatalf("snapshots=%d gets outside the open snapshot=%v, want none", snapshots, foreign)
		}
		if got := f.state(t).DispatchSkips["orphaned"]; got != 0 {
			t.Fatalf("DispatchSkips[orphaned] = %d, want 0", got)
		}
	})

	t.Run("an orphaned key", func(t *testing.T) {
		f := newOrphanSeatFixture(t)
		p := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
		f.enqueueFencedTo(t, p, "continue the claimed work", time.Now().Add(-time.Minute))
		f.closeSession(t, p.ID)
		f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
		f.fake.Calls = nil

		delivered, snapshots, foreign := pass(t, f)
		if delivered != 1 {
			t.Fatalf("delivered = %d, want 1", delivered)
		}
		if snapshots != 1 || len(foreign) != 1 || foreign[0] != p.ID {
			t.Fatalf("snapshots=%d gets outside the open snapshot=%v, want one snapshot and one lookup of %s", snapshots, foreign, p.ID)
		}
	})
}
