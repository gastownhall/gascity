package api

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// workLedgerStore stands in for a cross-region hosted work ledger: it counts
// the List calls the status path sends it, and can be told to stall so a read
// routed here cannot answer inside the caller's budget. The measured cost of a
// single work-store read on maintainer-city is 4.386s (54ms RTT over ~83
// serialized round trips, 87% network) against a 1s statusStoreReadTimeout, so
// "does not return in time" is the honest model — a latch pins that
// deterministically where a wall-clock sleep only approximates it.
type workLedgerStore struct {
	*beads.MemStore
	listCalls atomic.Int64
	stall     chan struct{} // nil answers immediately; non-nil parks until closed
}

func (s *workLedgerStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls.Add(1)
	if s.stall != nil {
		<-s.stall
	}
	return s.MemStore.List(query)
}

// stallWorkLedger makes every work-ledger read outlive the status read budget,
// releasing parked readers at cleanup so no goroutine outlives the test.
func stallWorkLedger(t *testing.T, work *workLedgerStore) {
	t.Helper()
	work.stall = make(chan struct{})
	t.Cleanup(func() { close(work.stall) })
}

// sessionClassUnavailableState reports a work store but no session-class store,
// the shape a city gets when [beads.classes.sessions] is relocated to a binding
// that failed to open.
type sessionClassUnavailableState struct{ *fakeState }

func (sessionClassUnavailableState) SessionsBeadStore() beads.SessionStore {
	return beads.SessionStore{}
}

// splitClassState builds a city with sessions relocated off the work ledger:
// distinct work and session-class stores, the way a converged split city binds
// them. The work ledger answers immediately unless a test stalls it, so the
// tests that assert routing can still observe what a work-ledger read WOULD
// have returned.
func splitClassState(t *testing.T) (*fakeState, *workLedgerStore, *beads.MemStore) {
	t.Helper()
	work := &workLedgerStore{MemStore: beads.NewMemStore()}
	sessions := beads.NewMemStore()
	st := newFakeState(t)
	st.cityBeadStore = work
	st.sessionsBeadStore = sessions
	// Drop the rig store so no unrelated leg reads while these tests run.
	st.stores = map[string]beads.Store{}
	st.cfg.Rigs = nil
	return st, work, sessions
}

// TestStatusSessionSnapshotReadsSessionClassStore is the routing test: on a
// split city the snapshot must see the beads in the session-class store and
// must not see the work ledger's, because session beads are not work beads.
func TestStatusSessionSnapshotReadsSessionClassStore(t *testing.T) {
	st, work, sessions := splitClassState(t)
	if _, err := work.Create(newSessionBead("work-ledger-session")); err != nil {
		t.Fatalf("Create work-ledger session bead: %v", err)
	}
	if _, err := sessions.Create(newSessionBead("session-class-session")); err != nil {
		t.Fatalf("Create session-class session bead: %v", err)
	}
	s := &Server{state: st}

	snapshot := s.statusSessionSnapshot(context.Background())

	if _, ok := snapshot.bySessionName["session-class-session"]; !ok {
		t.Errorf("snapshot missing session-class-session; bySessionName = %+v, want the session-class store read", snapshot.bySessionName)
	}
	if _, ok := snapshot.bySessionName["work-ledger-session"]; ok {
		t.Errorf("snapshot contains work-ledger-session; bySessionName = %+v, want session beads read off the session class, not the work ledger", snapshot.bySessionName)
	}
}

// TestStatusSessionSnapshotNeverQueriesTheWorkLedger is the cost test. The work
// ledger is stalled the way maintainer-city's is effectively stalled — a read
// there costs multiples of statusStoreReadTimeout — so a session read routed to
// it cannot finish and /status reports "sessions: loading session snapshot
// timed out after 1s" for data sitting in a local store. Zero work-ledger List
// calls pins the win structurally; the elapsed check pins that the snapshot no
// longer waits out the budget.
func TestStatusSessionSnapshotNeverQueriesTheWorkLedger(t *testing.T) {
	st, work, sessions := splitClassState(t)
	stallWorkLedger(t, work)
	if _, err := sessions.Create(newSessionBead("session-class-session")); err != nil {
		t.Fatalf("Create session-class session bead: %v", err)
	}
	s := &Server{state: st}

	start := time.Now()
	snapshot := s.statusSessionSnapshot(context.Background())
	elapsed := time.Since(start)

	if calls := work.listCalls.Load(); calls != 0 {
		t.Errorf("work ledger received %d List call(s) for a session-class read; want 0", calls)
	}
	if elapsed >= statusStoreReadTimeout {
		t.Errorf("statusSessionSnapshot took %s, at or past the %s budget; want a local session-class read", elapsed, statusStoreReadTimeout)
	}
	if len(snapshot.partialErrors) != 0 {
		t.Errorf("partialErrors = %v, want none", snapshot.partialErrors)
	}
	if _, ok := snapshot.bySessionName["session-class-session"]; !ok {
		t.Errorf("snapshot missing session-class-session; bySessionName = %+v", snapshot.bySessionName)
	}
}

// TestStatusSessionSnapshotStoreIdentity pins WHICH store value the read path
// is handed, in both city shapes. On a city that relocates nothing the
// session-class accessor resolves to the very same store value CityBeadStore
// returns, so this read is byte-identical there; on a split city it must be the
// session-class store and nothing else. ScopedStoreLike sees the store the read
// is about to go through, which makes it the honest observation point.
func TestStatusSessionSnapshotStoreIdentity(t *testing.T) {
	t.Run("single store", func(t *testing.T) {
		st := newFakeState(t)
		city := beads.NewMemStore()
		st.cityBeadStore = city
		var observed beads.Store
		st.scopedStoreFn = func(_ context.Context, existing beads.Store) (beads.Store, error) {
			observed = existing
			return nil, nil
		}
		s := &Server{state: st}

		_ = s.statusSessionSnapshot(context.Background())

		if observed != st.CityBeadStore() {
			t.Errorf("read store = %p, want CityBeadStore %p — a city that relocates nothing must stay byte-identical", observed, st.CityBeadStore())
		}
		if observed != st.SessionsBeadStore().Store {
			t.Errorf("read store = %p, want SessionsBeadStore %p", observed, st.SessionsBeadStore().Store)
		}
	})

	t.Run("sessions relocated", func(t *testing.T) {
		st, work, sessions := splitClassState(t)
		var observed beads.Store
		st.scopedStoreFn = func(_ context.Context, existing beads.Store) (beads.Store, error) {
			observed = existing
			return nil, nil
		}
		s := &Server{state: st}

		_ = s.statusSessionSnapshot(context.Background())

		if observed != beads.Store(sessions) {
			t.Errorf("read store = %p, want the session-class store %p (work ledger is %p)", observed, sessions, work)
		}
	})
}

// TestStatusSessionSnapshotFailsLoudWithoutSessionClassStore pins Invariant 0
// for this projection: when the work store is present but the session class is
// not reachable, /status must say it cannot see the class. Falling back to the
// work ledger is what made the mis-routing invisible in the first place, so the
// work ledger's session bead must not appear and must not be queried.
func TestStatusSessionSnapshotFailsLoudWithoutSessionClassStore(t *testing.T) {
	st, work, _ := splitClassState(t)
	if _, err := work.Create(newSessionBead("work-ledger-session")); err != nil {
		t.Fatalf("Create work-ledger session bead: %v", err)
	}
	s := &Server{state: sessionClassUnavailableState{st}}

	snapshot := s.statusSessionSnapshot(context.Background())

	joined := strings.Join(snapshot.partialErrors, "; ")
	if !strings.Contains(joined, "sessions: session-class bead store unavailable") {
		t.Errorf("partialErrors = %v, want the session-class store reported unavailable", snapshot.partialErrors)
	}
	if _, ok := snapshot.bySessionName["work-ledger-session"]; ok {
		t.Errorf("snapshot contains work-ledger-session; bySessionName = %+v, want no silent fallback to the work ledger", snapshot.bySessionName)
	}
	if calls := work.listCalls.Load(); calls != 0 {
		t.Errorf("work ledger received %d List call(s) after the session class went unavailable; want 0", calls)
	}
}

// TestStatusSessionSnapshotSilentWhenNoStoreConfigured keeps the beadless city
// quiet: with no store at all there is no class to be unable to see, so the
// snapshot stays empty and reports nothing, exactly as before.
func TestStatusSessionSnapshotSilentWhenNoStoreConfigured(t *testing.T) {
	st := newFakeState(t) // cityBeadStore left nil
	s := &Server{state: st}

	snapshot := s.statusSessionSnapshot(context.Background())

	if len(snapshot.partialErrors) != 0 {
		t.Errorf("partialErrors = %v, want none for a city with no bead store", snapshot.partialErrors)
	}
	if len(snapshot.bySessionName) != 0 {
		t.Errorf("bySessionName = %+v, want empty", snapshot.bySessionName)
	}
}

// TestStatusBodyNamedSessionUsesSessionClassStore carries the routing all the
// way to the wire: a named session materialized in the session-class store must
// render its persisted state, not "reserved-unmaterialized", on a split city.
func TestStatusBodyNamedSessionUsesSessionClassStore(t *testing.T) {
	st, work, sessions := splitClassState(t)
	identity := st.cfg.NamedSessions[0].QualifiedName()
	runtimeName := config.NamedSessionRuntimeName(st.cityName, st.cfg.Workspace, identity)
	if _, err := sessions.Create(newSessionBead(runtimeName)); err != nil {
		t.Fatalf("Create named-session bead: %v", err)
	}
	// A decoy under a different name on the work ledger: reading there answers
	// "reserved-unmaterialized" for the named session, which is the pre-fix body.
	if _, err := work.Create(newSessionBead("work-ledger-session")); err != nil {
		t.Fatalf("Create work-ledger session bead: %v", err)
	}
	s := &Server{state: st}

	body := s.buildStatusBody(context.Background(), false)

	if len(body.NamedSessionDetails) != 1 {
		t.Fatalf("NamedSessionDetails = %+v, want exactly one row", body.NamedSessionDetails)
	}
	if got := body.NamedSessionDetails[0].Status; got != string(session.StateActive) {
		t.Errorf("named session %q status = %q, want %q — the session-class store holds it", identity, got, session.StateActive)
	}
	if body.SessionCountsDetail == nil || body.SessionCountsDetail.Active != 1 {
		t.Errorf("SessionCountsDetail = %+v, want Active=1 from the session-class store", body.SessionCountsDetail)
	}
}
