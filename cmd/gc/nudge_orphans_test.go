package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
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
	if _, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, nil, &debugOut); err != nil {
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

	if _, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, nil, nil); err != nil {
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
// sessions are created through the production session manager. Its passes
// share one predecessor memo, as the controller's do.
type orphanSeatFixture struct {
	dir          string
	store        beads.NudgesStore
	fake         *runtime.Fake
	mgr          *session.Manager
	predecessors nudgeOrphanPredecessorMemo
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
	delivered, err := dispatchAllQueuedNudges(f.dir, supervisorCfg(), f.store.Store, f.store.Store, sp, snapshot, &f.predecessors, w)
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
// fence; an item already fenced to an open session that holds its fence
// resolves to that session. Every resolution other than successor leaves the
// key queued.
func TestResolveNudgeOrphanSeat(t *testing.T) {
	cases := []struct {
		name string
		// build returns the item to resolve, the lookup budget, and the
		// expected successor session ID ("" for none).
		build          func(t *testing.T, f *orphanSeatFixture) (item queuedNudge, budget int, wantSuccessor string)
		wantResolution nudgeOrphanResolution
		// hideFence leaves the item's fenced session out of the open
		// sessions, as a read older than that session would.
		hideFence bool
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
			name: "an item a claim re-fenced to the open seat holder resolves to that holder",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				f.closeSession(t, p.ID)
				s := f.createSeat(t, "worker-2", "2")
				item := fencedOrphanItem(p)
				item.SessionID, item.ContinuationEpoch = s.ID, s.ContinuationEpoch
				return item, nudgeOrphanPredecessorLookupsPerPass, s.ID
			},
			wantResolution: nudgeOrphanSuccessor,
		},
		{
			name: "fenced session is open and holds its fence but the open read does not list it",
			build: func(t *testing.T, f *orphanSeatFixture) (queuedNudge, int, string) {
				p := f.createSeat(t, "worker-2", "2")
				return fencedOrphanItem(p), nudgeOrphanPredecessorLookupsPerPass, ""
			},
			wantResolution: nudgeOrphanFencedSessionLive,
			hideFence:      true,
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
			var open []session.Info
			for _, info := range snapshot.OpenInfos() {
				if !tc.hideFence || info.ID != item.SessionID {
					open = append(open, info)
				}
			}
			seat := newNudgeOrphanSeatResolver(open, sessionFrontDoor(f.store.Store).Get, budget).resolve(item)
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

// countingPass runs one dispatch pass and returns what it delivered, how many
// whole-queue snapshots it read, and the session Gets it made outside the
// open snapshot (by ID or session name).
func (f *orphanSeatFixture) countingPass(t *testing.T) (delivered int, snapshots int, foreignGets []string) {
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
	delivered, err = dispatchQueuedNudges(handle, f.dir, supervisorCfg(), f.store.Store, sessStore, f.fake, snapshot, &f.predecessors, nil)
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

// A pass whose every due key an open session lists reads no whole-queue
// snapshot and looks up no session outside the open snapshot (by ID or
// session name); a pass with an
// orphaned key reads the snapshot once and looks up its fenced predecessor.
func TestDispatchSteadyStatePassReadsNoOrphanState(t *testing.T) {
	t.Run("every due key matched", func(t *testing.T) {
		f := newOrphanSeatFixture(t)
		s := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
		f.enqueueFencedTo(t, s, "keep going", time.Now().Add(-time.Minute))
		f.startIdle(t, f.createSeat(t, "worker-9", "9").ID)
		f.fake.Calls = nil

		delivered, snapshots, foreign := f.countingPass(t)
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

		delivered, snapshots, foreign := f.countingPass(t)
		if delivered != 1 {
			t.Fatalf("delivered = %d, want 1", delivered)
		}
		if snapshots != 1 || len(foreign) != 1 || foreign[0] != p.ID {
			t.Fatalf("snapshots=%d gets outside the open snapshot=%v, want one snapshot and one lookup of %s", snapshots, foreign, p.ID)
		}
	})
}

// An adopted orphan whose claimed delivery does not succeed is left re-fenced
// to the successor but still under the predecessor's key. A later due pass
// hands that key to the successor again and delivers the item to it exactly
// once. Each case leaves the item so through one production path: a refusal
// the claim releases for a retry, a lapsed in-flight lease (a controller that
// died mid-delivery), and a second delivery group the first pass defers.
func TestDispatchRetriesAdoptedOrphanAfterUndeliveredClaim(t *testing.T) {
	enqueueOne := func(t *testing.T, f *orphanSeatFixture, p session.Info) {
		t.Helper()
		f.enqueueFencedTo(t, p, "continue the claimed work", time.Now().Add(-time.Minute))
	}
	refusedBy := func(err error) func(t *testing.T, f *orphanSeatFixture, s session.Info) {
		return func(t *testing.T, f *orphanSeatFixture, s session.Info) {
			t.Helper()
			f.fake.NudgeErrors = map[string]error{s.SessionName: err}
			delivered := f.dispatch(t, f.fake, nil)
			if msgs := f.nudgeCalls(); delivered != 0 || len(msgs) != 1 || !strings.HasPrefix(msgs[0], s.SessionName+":") {
				t.Fatalf("refused first pass: delivered=%d nudges=%v, want one undelivered attempt on %s", delivered, msgs, s.SessionName)
			}
			f.fake.NudgeErrors = nil
		}
	}
	cases := []struct {
		name    string
		enqueue func(t *testing.T, f *orphanSeatFixture, p session.Info)
		// interrupt leaves the adopted item undelivered, re-fenced to s.
		interrupt func(t *testing.T, f *orphanSeatFixture, s session.Info)
		// retried is the message the later pass must deliver.
		retried string
	}{
		{name: "the successor is busy", enqueue: enqueueOne, interrupt: refusedBy(runtime.ErrSessionBusy), retried: "continue the claimed work"},
		{name: "the successor refuses while an interaction is pending", enqueue: enqueueOne, interrupt: refusedBy(runtime.ErrNudgeRefusedPendingInteraction), retried: "continue the claimed work"},
		{
			name:    "the in-flight lease lapsed",
			enqueue: enqueueOne,
			interrupt: func(t *testing.T, f *orphanSeatFixture, s session.Info) {
				t.Helper()
				// What a claim that died mid-delivery leaves behind.
				target := resolveNudgeTargetFromSessionInfo(f.dir, supervisorCfg(), s)
				now := time.Now()
				if err := updateNudgeQueueState(f.dir, "test-lapsed-claim", func(state *nudgeQueueState) error {
					for _, item := range state.Pending {
						item = refencedQueuedNudge(target, item)
						item.DeliveryBatch = newQueuedNudgeBatchID()
						item.ClaimedAt = now.Add(-2 * defaultQueuedNudgeClaimTTL).UTC()
						item.LeaseUntil = now.Add(-defaultQueuedNudgeClaimTTL).UTC()
						state.InFlight = append(state.InFlight, item)
					}
					state.Pending = nil
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
			retried: "continue the claimed work",
		},
		{
			name: "a second delivery group waits for a later pass",
			enqueue: func(t *testing.T, f *orphanSeatFixture, p session.Info) {
				t.Helper()
				f.enqueueFencedTo(t, p, "first reminder", time.Now().Add(-2*time.Minute))
				submit := newQueuedNudgeWithOptions(p.ID, "then the user's prompt", nudgequeue.SourceSubmit, time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: p.ID, ContinuationEpoch: p.ContinuationEpoch})
				if err := enqueueQueuedNudge(f.dir, submit); err != nil {
					t.Fatal(err)
				}
			},
			interrupt: func(t *testing.T, f *orphanSeatFixture, s session.Info) {
				t.Helper()
				delivered := f.dispatch(t, f.fake, nil)
				if msgs := f.nudgeCalls(); delivered != 1 || len(msgs) != 1 || !strings.HasPrefix(msgs[0], s.SessionName+":") || !strings.Contains(msgs[0], "first reminder") {
					t.Fatalf("first pass: delivered=%d nudges=%v, want only the first reminder delivered to %s", delivered, msgs, s.SessionName)
				}
			},
			retried: "then the user's prompt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrphanSeatFixture(t)
			p := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
			tc.enqueue(t, f, p)
			f.closeSession(t, p.ID)
			s := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
			f.fake.Calls = nil

			tc.interrupt(t, f, s)
			state := f.state(t)
			var left []queuedNudge
			for _, bucket := range [][]queuedNudge{state.Pending, state.InFlight} {
				for _, item := range bucket {
					if item.Agent == p.ID {
						left = append(left, item)
					}
				}
			}
			if len(left) != 1 || left[0].SessionID != s.ID || left[0].Message != tc.retried {
				t.Fatalf("queued under %s after the first attempt = %+v, want only %q, re-fenced to %s", p.ID, left, tc.retried, s.ID)
			}

			// Let the retry delay pass and the successor go idle again.
			if err := updateNudgeQueueState(f.dir, "test-retry-due", func(state *nudgeQueueState) error {
				for i := range state.Pending {
					state.Pending[i].DeliverAfter = time.Now().Add(-time.Second).UTC()
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			f.fake.Activity[s.SessionName] = time.Now().Add(-10 * time.Second)
			f.fake.Calls = nil
			delivered := f.dispatch(t, f.fake, nil)
			if msgs := f.nudgeCalls(); delivered != 1 || len(msgs) != 1 || !strings.HasPrefix(msgs[0], s.SessionName+":") || !strings.Contains(msgs[0], tc.retried) {
				t.Fatalf("retry pass: delivered=%d nudges=%v, want %q delivered once to successor %s", delivered, msgs, tc.retried, s.SessionName)
			}
			state = f.state(t)
			for _, bucket := range [][]queuedNudge{state.Pending, state.InFlight} {
				for _, item := range bucket {
					if item.Agent == p.ID {
						t.Fatalf("item %s still queued under the predecessor's key: %+v", item.ID, item)
					}
				}
			}
			if got := state.DispatchSkips["orphaned"]; got != 0 {
				t.Fatalf("DispatchSkips[orphaned] = %d, want 0: the key went to the successor on every pass; full map %#v", got, state.DispatchSkips)
			}
			f.fake.Activity[s.SessionName] = time.Now().Add(-10 * time.Second)
			f.fake.Calls = nil
			if delivered := f.dispatch(t, f.fake, nil); delivered != 0 || len(f.nudgeCalls()) != 0 {
				t.Fatalf("a further pass delivered %d (%v), want nothing", delivered, f.nudgeCalls())
			}
		})
	}
}

// A pass reads at most nudgeOrphanPredecessorLookupsPerPass fenced sessions,
// newest items first, and carries the closed rows it read to the next pass. So
// an adoptable orphan older than that many unadoptable ones (closed
// predecessors whose seats no session holds) is adopted on a later pass rather
// than deferred until it expires, and once every due fence is carried a pass
// reads none.
func TestDispatchOrphanLookupBudgetAdvancesAcrossPasses(t *testing.T) {
	f := newOrphanSeatFixture(t)
	now := time.Now()
	p := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
	adoptable := f.enqueueFencedTo(t, p, "continue the claimed work", now.Add(-2*time.Hour))
	f.closeSession(t, p.ID)
	vacant := nudgeOrphanPredecessorLookupsPerPass + 1
	for i := 0; i < vacant; i++ {
		v := f.createSeat(t, fmt.Sprintf("worker-v%d", i), "1")
		f.enqueueFencedTo(t, v, "left for a vacant seat", now.Add(-time.Hour+time.Duration(i)*time.Second))
		f.closeSession(t, v.ID)
	}
	s := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
	f.fake.Calls = nil

	delivered, _, reads := f.countingPass(t)
	if delivered != 0 || len(reads) != nudgeOrphanPredecessorLookupsPerPass || slices.Contains(reads, p.ID) {
		t.Fatalf("pass 1: delivered=%d reads=%d (%v), want nothing delivered and the %d newest fences read, not %s",
			delivered, len(reads), reads, nudgeOrphanPredecessorLookupsPerPass, p.ID)
	}
	delivered, _, reads = f.countingPass(t)
	msgs := f.nudgeCalls()
	if delivered != 1 || len(reads) != 2 || !slices.Contains(reads, p.ID) || len(msgs) != 1 || !strings.HasPrefix(msgs[0], s.SessionName+":") || !strings.Contains(msgs[0], "continue the claimed work") {
		t.Fatalf("pass 2: delivered=%d reads=%v nudges=%v, want only the two unread fences read and %s adopted by %s",
			delivered, reads, msgs, adoptable.ID, s.SessionName)
	}
	delivered, _, reads = f.countingPass(t)
	if delivered != 0 || len(reads) != 0 {
		t.Fatalf("pass 3: delivered=%d reads=%v, want no fence read once every due one is carried", delivered, reads)
	}
	if pending := f.state(t).Pending; len(pending) != vacant {
		t.Fatalf("pending = %d items, want the %d vacant-seat items still queued", len(pending), vacant)
	}
}

// A resolver answers an earlier pass's settled reads (a closed row, an ID no
// record answers) without a lookup and reads again what was not settled (a
// failed read, an open row the open sessions did not list); a carried row the
// open sessions now list is answered from them. The memo keeps only the latest
// pass's settled reads.
func TestNudgeOrphanPredecessorMemoCarriesSettledReads(t *testing.T) {
	records := map[string]session.Info{
		"closed":   {ID: "closed", Closed: true, Template: "worker", AgentName: "worker-2", PoolSlot: "2", PoolManaged: true},
		"reopened": {ID: "reopened", Closed: true, Template: "worker", AgentName: "worker-3", PoolSlot: "3", PoolManaged: true},
		"unlisted": {ID: "unlisted", Template: "worker", AgentName: "worker-4", PoolSlot: "4", PoolManaged: true},
	}
	var reads []string
	lookup := func(id string) (session.Info, error) {
		reads = append(reads, id)
		if id == "failing" {
			return session.Info{}, errors.New("store unavailable")
		}
		if info, ok := records[id]; ok {
			return info, nil
		}
		return session.Info{}, beads.ErrNotFound
	}
	var memo nudgeOrphanPredecessorMemo
	pass := func(open []session.Info, fences ...string) map[string]nudgeOrphanPredecessor {
		reads = nil
		r := newNudgeOrphanSeatResolver(open, lookup, nudgeOrphanPredecessorLookupsPerPass)
		r.carried = memo.carried()
		got := make(map[string]nudgeOrphanPredecessor, len(fences))
		for _, id := range fences {
			got[id] = r.predecessor(id)
		}
		memo.keep(r.predecessors)
		return got
	}
	fences := []string{"closed", "missing", "failing", "unlisted", "reopened"}

	pass(nil, fences...)
	if !slices.Equal(reads, fences) {
		t.Fatalf("pass 1 reads = %v, want every fence read once: %v", reads, fences)
	}
	reopened := records["reopened"]
	reopened.Closed = false
	got := pass([]session.Info{reopened}, fences...)
	if want := []string{"failing", "unlisted"}; !slices.Equal(reads, want) {
		t.Fatalf("pass 2 reads = %v, want only the unsettled fences %v read again", reads, want)
	}
	if p := got["closed"]; p.info.ID != "closed" || !p.info.Closed || p.resolution != "" {
		t.Fatalf("carried closed row = %+v, want the closed record", p)
	}
	if p := got["missing"]; p.resolution != nudgeOrphanFencedSessionUnknown {
		t.Fatalf("carried missing fence = %+v, want fenced-session-unknown", p)
	}
	if p := got["reopened"]; !p.open || p.info.Closed {
		t.Fatalf("reopened fence = %+v, want the open session, not the carried closed row", p)
	}
	if p := got["failing"]; p.resolution != nudgeOrphanLookupError || p.err == nil {
		t.Fatalf("failing fence = %+v, want a lookup error", p)
	}
	pass(nil, "closed")
	if len(reads) != 0 {
		t.Fatalf("pass 3 reads = %v, want none", reads)
	}
	if carried := memo.carried(); len(carried) != 1 || carried["closed"].info.ID != "closed" {
		t.Fatalf("memo after pass 3 = %+v, want only the fence pass 3 named", carried)
	}
	memo.forget()
	if carried := memo.carried(); len(carried) != 0 {
		t.Fatalf("memo after forget = %+v, want empty", carried)
	}
	var none *nudgeOrphanPredecessorMemo
	none.keep(map[string]nudgeOrphanPredecessor{"closed": {settled: true}})
	if carried := none.carried(); carried != nil {
		t.Fatalf("nil memo carried %+v, want nothing", carried)
	}
}

// decideNudgeOrphanKeys hands a key to a successor only when every fenced item
// under it resolves to the same one and it carries no unfenced startup turn;
// it spends a lookup budget on the newest items first, and reports each failed
// fenced-session read once.
func TestDecideNudgeOrphanKeys(t *testing.T) {
	seat := func(id, agentName string, closed bool) session.Info {
		return session.Info{ID: id, Closed: closed, Template: "worker", AgentName: agentName, PoolSlot: "1", PoolManaged: true, SessionName: "s-" + id}
	}
	open := []session.Info{seat("succ-a", "worker-a", false), seat("succ-b", "worker-b", false)}
	records := map[string]session.Info{
		"pred-a":  seat("pred-a", "worker-a", true),
		"pred-a2": seat("pred-a2", "worker-a", true),
		"pred-b":  seat("pred-b", "worker-b", true),
	}
	lookup := func(id string) (session.Info, error) {
		if id == "pred-err" {
			return session.Info{}, errors.New("store unavailable")
		}
		if info, ok := records[id]; ok {
			return info, nil
		}
		return session.Info{}, beads.ErrNotFound
	}
	base := time.Now().Add(-time.Minute)
	n := 0
	item := func(key, fence string, age time.Duration) queuedNudge {
		n++
		return queuedNudge{ID: fmt.Sprintf("nudge-%d", n), Agent: key, SessionID: fence, Source: "hook-claim-continuation", CreatedAt: base.Add(-age)}
	}
	unfencedStartupTurn := func(key string) queuedNudge {
		turn := item(key, "", 0)
		turn.Source = nudgequeue.SourceSubmit
		turn.Reference = nudgequeue.StartupTurnReference("launch-1")
		return turn
	}
	type want struct {
		key        string
		resolution nudgeOrphanResolution
		successor  string
	}
	cases := []struct {
		name    string
		budget  int
		items   []queuedNudge
		want    []want
		wantErr int
	}{
		{
			name:   "items of two predecessors of one seat go to its holder",
			budget: nudgeOrphanPredecessorLookupsPerPass,
			items:  []queuedNudge{item("k", "pred-a", time.Minute), item("k", "pred-a2", 0)},
			want:   []want{{"k", nudgeOrphanSuccessor, "succ-a"}},
		},
		{
			name:   "items whose seats have different holders are ambiguous",
			budget: nudgeOrphanPredecessorLookupsPerPass,
			items:  []queuedNudge{item("k", "pred-a", time.Minute), item("k", "pred-b", 0)},
			want:   []want{{"k", nudgeOrphanSeatAmbiguous, ""}},
		},
		{
			name:   "an unfenced startup turn keeps a resolvable key",
			budget: nudgeOrphanPredecessorLookupsPerPass,
			items:  []queuedNudge{unfencedStartupTurn("k"), item("k", "pred-a", time.Minute)},
			want:   []want{{"k", nudgeOrphanStartupTurn, ""}},
		},
		{
			name:    "a failed read is a lookup error, reported once per fence",
			budget:  nudgeOrphanPredecessorLookupsPerPass,
			items:   []queuedNudge{item("k1", "pred-err", time.Minute), item("k1", "pred-err", 0), item("k2", "pred-err", 0)},
			want:    []want{{"k1", nudgeOrphanLookupError, ""}, {"k2", nudgeOrphanLookupError, ""}},
			wantErr: 1,
		},
		{
			name:   "the budget goes to the newest items first",
			budget: 1,
			items:  []queuedNudge{item("k-old", "pred-b", time.Hour), item("k-new", "pred-a", 0)},
			want:   []want{{"k-old", nudgeOrphanLookupDeferred, ""}, {"k-new", nudgeOrphanSuccessor, "succ-a"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var keys []string
			due := make(map[string][]queuedNudge)
			for _, it := range tc.items {
				if _, ok := due[it.Agent]; !ok {
					keys = append(keys, it.Agent)
				}
				due[it.Agent] = append(due[it.Agent], it)
			}
			decided, err := decideNudgeOrphanKeys(newNudgeOrphanSeatResolver(open, lookup, tc.budget), keys, due)
			var errs []error
			if err != nil {
				errs = err.(interface{ Unwrap() []error }).Unwrap()
			}
			if len(errs) != tc.wantErr {
				t.Fatalf("errors = %v, want %d", errs, tc.wantErr)
			}
			if len(decided) != len(tc.want) {
				t.Fatalf("decided = %+v, want %d keys", decided, len(tc.want))
			}
			for i, w := range tc.want {
				if got := decided[i]; got.Key != w.key || got.Resolution != w.resolution || got.Successor.ID != w.successor {
					t.Fatalf("key %d = {%s %s %s}, want {%s %s %s}", i, got.Key, got.Resolution, got.Successor.ID, w.key, w.resolution, w.successor)
				}
			}
		})
	}
}

// inheritNudgeOrphanKey hands a key only to a successor target that can be
// delivered to: one without a session name leaves the key orphaned.
func TestInheritNudgeOrphanKeyNeedsADeliverableTarget(t *testing.T) {
	k := nudgeOrphanKey{Key: "pred-a", Resolution: nudgeOrphanSuccessor, Successor: session.Info{ID: "succ-a"}}
	targets := []nudgeTarget{{sessionID: "other", sessionName: "s-other"}, {sessionID: "succ-a"}}
	if name, ok := inheritNudgeOrphanKey(targets, k); ok || name != "" || len(targets[1].inheritedKeys) != 0 {
		t.Fatalf("inherit to a target with no session name = %q, %v, keys %v; want refused", name, ok, targets[1].inheritedKeys)
	}
	targets[1].sessionName = "s-succ-a"
	if name, ok := inheritNudgeOrphanKey(targets, k); !ok || name != "s-succ-a" || !slices.Equal(targets[1].inheritedKeys, []string{"pred-a"}) || len(targets[0].inheritedKeys) != 0 {
		t.Fatalf("inherit = %q, %v, keys %v / %v; want only s-succ-a handed pred-a", name, ok, targets[1].inheritedKeys, targets[0].inheritedKeys)
	}
}

// failingSnapshotQueue is a queue whose whole-queue Snapshot read fails.
type failingSnapshotQueue struct {
	storebinding.NudgeQueue
	err error
}

func (q failingSnapshotQueue) Snapshot() (nudgequeue.State, error) {
	return nudgequeue.State{}, q.err
}

// A pass that cannot read the queue counts every orphaned key a lookup error,
// returns the read error, and leaves the carried reads as they were.
func TestResolveNudgeOrphanKeysForPassCannotReadQueue(t *testing.T) {
	handle := openNudgeQueue(t.TempDir(), beads.NudgesStore{})
	handle.Queue = failingSnapshotQueue{NudgeQueue: handle.Queue, err: errors.New("queue unreadable")}
	var memo nudgeOrphanPredecessorMemo
	memo.keep(map[string]nudgeOrphanPredecessor{"pred-a": {info: session.Info{ID: "pred-a", Closed: true}, settled: true}})

	decided, err := resolveNudgeOrphanKeysForPass(handle, []string{"k1", "k2"}, nil, nil, &memo, time.Now())
	if err == nil || !strings.Contains(err.Error(), "queue unreadable") {
		t.Fatalf("err = %v, want the queue read error", err)
	}
	if len(decided) != 2 || decided[0].Key != "k1" || decided[1].Key != "k2" || decided[0].Resolution != nudgeOrphanLookupError || decided[1].Resolution != nudgeOrphanLookupError {
		t.Fatalf("decided = %+v, want k1 and k2 counted lookup-error", decided)
	}
	if carried := memo.carried(); len(carried) != 1 || carried["pred-a"].info.ID != "pred-a" {
		t.Fatalf("memo = %+v, want its read kept", carried)
	}
}
