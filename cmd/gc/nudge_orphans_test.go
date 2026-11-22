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

// createSeat creates a session of template holding a pool seat: pool-managed
// with agentName and slot. An empty slot creates a manual (non-pool) session.
func (f *orphanSeatFixture) createSeat(t *testing.T, template, agentName, slot string) session.Info {
	t.Helper()
	meta := map[string]string{"session_origin": "manual", "agent_name": agentName}
	if slot != "" {
		meta["session_origin"] = "ephemeral"
		meta["pool_managed"] = "true"
		meta["pool_slot"] = slot
	}
	info, err := f.mgr.CreateSession(context.Background(), session.CreateOptions{Template: template, Title: template, Command: "codex", WorkDir: f.dir, Provider: "codex", Hints: runtime.Config{WorkDir: f.dir}, ExtraMeta: meta})
	if err != nil {
		t.Fatalf("CreateSession %s/%s: %v", template, agentName, err)
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
	p := f.createSeat(t, "worker", "worker-2", "2")
	p = f.startIdle(t, p.ID)
	item := f.enqueueFencedTo(t, p, "continue the claimed work", time.Now().Add(-time.Minute))
	f.closeSession(t, p.ID)
	closed := f.get(t, p.ID)
	if !closed.Closed || closed.AgentName != "worker-2" || closed.Template != "worker" || closed.PoolSlot != "2" || !isPoolManagedSessionInfo(closed) {
		t.Fatalf("setup: closed predecessor row = %+v, want it closed and keeping its pool seat", closed)
	}
	s := f.createSeat(t, "worker", "worker-2", "2")
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
