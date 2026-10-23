package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin #4703: the agent roster and the rig counts must probe a
// bounded-pool worker's liveness on the runtime session it actually runs
// under. The controller names an unaliased pool session after its session
// bead (<template>-<beadID>), never after the slot the roster enumerates
// (<pool>-1 ... <pool>-N), so a probe of the name derived from the slot
// always misses.

const (
	boundedPoolTemplate = "specs/archivist"
	liveSlot            = "specs/archivist-1"
	liveSlotSession     = "archivist-gc-41"
	deadSlot            = "specs/archivist-2"
	deadSlotSession     = "archivist-gc-42"
)

// boundedPoolState is a rig whose bounded pool (max_active_sessions = 2) has
// a session on each slot, each recorded on a session bead the way the
// controller writes one. Slot 1's runtime session is live. Slot 2's bead
// still says active, but its runtime is gone, as after a crash.
func boundedPoolState(t *testing.T) (*fakeState, *beads.MemStore) {
	t.Helper()
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "archivist", Dir: "specs", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
	})
	for _, seat := range []struct{ slot, sessionName string }{
		{slot: liveSlot, sessionName: liveSlotSession},
		{slot: deadSlot, sessionName: deadSlotSession},
	} {
		if _, err := sessions.Create(poolSessionBeadWithAgentName(boundedPoolTemplate, seat.slot, seat.sessionName)); err != nil {
			t.Fatalf("create session bead for %s: %v", seat.slot, err)
		}
		// The premise of #4703: no slot derives its runtime name.
		if derived := agentSessionName(st.cityName, seat.slot, ""); derived == seat.sessionName {
			t.Fatalf("slot %s derives its runtime name %q, so this fixture no longer covers #4703", seat.slot, derived)
		}
	}
	if err := st.sp.Start(context.Background(), liveSlotSession, runtime.Config{}); err != nil {
		t.Fatalf("start runtime session %s: %v", liveSlotSession, err)
	}
	return st, sessions
}

// agentListWire, getAgentList and agentByNameIn are the /agents list
// helpers main's handler_agents_configured_test.go defines; that file is not
// on this branch.
type agentListWire struct {
	Items         []agentResponse `json:"items"`
	Total         int             `json:"total"`
	Partial       bool            `json:"partial"`
	PartialErrors []string        `json:"partial_errors"`
}

func getAgentList(t *testing.T, state *fakeState, query string) agentListWire {
	t.Helper()
	h := newTestCityHandlerWith(t, state, New(state))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/agents"+query), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents%s status = %d, want 200; body: %s", query, rec.Code, rec.Body.String())
	}
	var resp agentListWire
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func agentByNameIn(items []agentResponse, name string) (agentResponse, bool) {
	for _, item := range items {
		if item.Name == name {
			return item, true
		}
	}
	return agentResponse{}, false
}

// requireAgentRow returns the row named name, failing the test without one.
func requireAgentRow(t *testing.T, items []agentResponse, name string) agentResponse {
	t.Helper()
	row, ok := agentByNameIn(items, name)
	if !ok {
		t.Fatalf("no agent row for %s; rows = %v", name, agentNames(items))
	}
	return row
}

func agentNames(items []agentResponse) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	return names
}

// checkLiveSlot asserts a row reads the live slot running under its runtime
// session, and checkDeadSlot that a row reads the crashed slot stopped: a
// session bead names a session, it is not evidence the session is alive.
func checkLiveSlot(t *testing.T, endpoint string, row agentResponse) {
	t.Helper()
	if !row.Running || row.State != "idle" {
		t.Errorf("%s: %s = {running:%v state:%q}, want {true idle}: its session %s is live",
			endpoint, liveSlot, row.Running, row.State, liveSlotSession)
	}
	if row.Session == nil || row.Session.Name != liveSlotSession {
		t.Errorf("%s: %s session = %+v, want %s", endpoint, liveSlot, row.Session, liveSlotSession)
	}
}

func checkDeadSlot(t *testing.T, endpoint string, row agentResponse) {
	t.Helper()
	if row.Running || row.State != "stopped" || row.Session != nil {
		t.Errorf("%s: %s = {running:%v state:%q session:%+v}, want {false stopped <nil>}: its bead says active, but its runtime is gone",
			endpoint, deadSlot, row.Running, row.State, row.Session)
	}
}

func TestAgentListReadsBoundedPoolWorkerOffItsRuntimeSession(t *testing.T) {
	st, _ := boundedPoolState(t)

	items := getAgentList(t, st, "").Items
	checkLiveSlot(t, "GET /agents", requireAgentRow(t, items, liveSlot))
	checkDeadSlot(t, "GET /agents", requireAgentRow(t, items, deadSlot))

	for _, tt := range []struct{ query, want string }{
		{query: "?running=true", want: liveSlot},
		{query: "?running=false", want: deadSlot},
	} {
		if items := getAgentList(t, st, tt.query).Items; len(items) != 1 || items[0].Name != tt.want {
			t.Errorf("GET /agents%s = %v, want only %s", tt.query, agentNames(items), tt.want)
		}
	}
}

func getAgent(t *testing.T, st *fakeState, name string) agentResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	newTestCityHandler(t, st).ServeHTTP(rec, httptest.NewRequest("GET", cityURL(st, "/agent/"+name), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agent/%s status = %d, want %d: %s", name, rec.Code, http.StatusOK, rec.Body.String())
	}
	var row agentResponse
	if err := json.NewDecoder(rec.Body).Decode(&row); err != nil {
		t.Fatalf("decode GET /agent/%s: %v", name, err)
	}
	return row
}

func TestAgentGetReadsBoundedPoolWorkerOffItsRuntimeSession(t *testing.T) {
	st, _ := boundedPoolState(t)
	checkLiveSlot(t, "GET /agent/"+liveSlot, getAgent(t, st, liveSlot))
	checkDeadSlot(t, "GET /agent/"+deadSlot, getAgent(t, st, deadSlot))
}

// TestAgentGetBoundedPoolOwnNameIsNoSeat pins that GET /agent/<pool> on a pool
// of several seats reads none of its seats' sessions, whether one seat is
// recorded or two: the pool's own name is no seat. The seat itself still reads
// running. On this branch the unlimited pool needs the same rule, since its
// statusRuntimeSessionName has no template-row guard.
func TestAgentGetBoundedPoolOwnNameIsNoSeat(t *testing.T) {
	oneSeat := func(maxSessions int) func(*testing.T) *fakeState {
		return func(t *testing.T) *fakeState {
			st, sessions := poolIdentityState(t, config.Agent{
				Name: "archivist", Dir: "specs", Provider: "test-agent",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(maxSessions),
			})
			if _, err := sessions.Create(poolSessionBeadWithAgentName(boundedPoolTemplate, liveSlot, liveSlotSession)); err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			if err := st.sp.Start(context.Background(), liveSlotSession, runtime.Config{}); err != nil {
				t.Fatalf("start runtime session %s: %v", liveSlotSession, err)
			}
			return st
		}
	}
	twoSeats := func(t *testing.T) *fakeState {
		st, _ := boundedPoolState(t)
		return st
	}
	for _, tt := range []struct {
		name  string
		state func(*testing.T) *fakeState
	}{
		{name: "one seat recorded", state: oneSeat(2)},
		{name: "two seats recorded", state: twoSeats},
		{name: "unlimited pool, one seat recorded", state: oneSeat(-1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := tt.state(t)
			if row := getAgent(t, st, boundedPoolTemplate); row.Running || row.Session != nil {
				t.Errorf("GET /agent/%s = {running:%v session:%+v}, want {false <nil>}: the pool's own name is none of its seats",
					boundedPoolTemplate, row.Running, row.Session)
			}
			checkLiveSlot(t, "GET /agent/"+liveSlot, getAgent(t, st, liveSlot))
		})
	}
}

func TestRigRunningCountCountsBoundedPoolWorker(t *testing.T) {
	st, _ := boundedPoolState(t)
	h := newTestCityHandler(t, st)

	var rig rigResponse
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(st, "/rig/specs"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /rig/specs status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode GET /rig/specs: %v", err)
	}
	if rig.AgentCount != 2 || rig.RunningCount != 1 {
		t.Errorf("GET /rig/specs = {agent_count:%d running_count:%d}, want {2 1}: one pool worker is live", rig.AgentCount, rig.RunningCount)
	}

	var list ListBody[rigResponse]
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(st, "/rigs"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /rigs status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode GET /rigs: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].AgentCount != 2 || list.Items[0].RunningCount != 1 {
		t.Errorf("GET /rigs = %+v, want one rig with {agent_count:2 running_count:1}", list.Items)
	}
}

// TestAgentListReadsControlDispatcherOffItsRuntimeSession is the issue's
// control-dispatcher row: a single-seat pool session under a bead-scoped name,
// which reads running, with no provider.
func TestAgentListReadsControlDispatcherOffItsRuntimeSession(t *testing.T) {
	st, sessions := poolIdentityState(t, controlDispatcherAgent(t, "specs"))
	st.cfg.Workspace.Provider = "claude"
	qualifiedName := "specs/" + config.ControlDispatcherAgentName
	sessionName := config.ControlDispatcherAgentName + "-gc-7"
	if _, err := sessions.Create(poolSessionBeadWithAgentName(qualifiedName, qualifiedName, sessionName)); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := st.sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("start runtime session %s: %v", sessionName, err)
	}
	srv := New(st)
	srv.LookPathFunc = lookPathNotFound
	h := newTestCityHandlerWith(t, st, srv)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(st, "/agents"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var list agentListWire
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode GET /agents: %v", err)
	}
	row := requireAgentRow(t, list.Items, qualifiedName)
	if !row.Running || row.Session == nil || row.Session.Name != sessionName {
		t.Errorf("%s = {running:%v session:%+v}, want running on %s", qualifiedName, row.Running, row.Session, sessionName)
	}
	if row.Provider != "" || !row.Available {
		t.Errorf("%s = {provider:%q available:%v}, want {\"\" true}", qualifiedName, row.Provider, row.Available)
	}
}

// sessionCensusCountingStore counts the session snapshot reads made through
// it: session.ListAllSessionBeads lists the session beads by type once per
// read.
type sessionCensusCountingStore struct {
	beads.Store
	censuses int
}

func (s *sessionCensusCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == session.BeadType {
		s.censuses++
	}
	return s.Store.List(query)
}

// TestAgentListReadsSessionSnapshotOnlyOnAMiss pins the cost of the join: one
// session snapshot per request however many slots miss, and none when every
// agent runs under the name derived from it.
func TestAgentListReadsSessionSnapshotOnlyOnAMiss(t *testing.T) {
	st, sessions := boundedPoolState(t)
	counting := &sessionCensusCountingStore{Store: sessions}
	st.sessionsBeadStore = counting
	getAgentList(t, st, "")
	if counting.censuses != 1 {
		t.Errorf("session snapshot reads for two missed slots = %d, want 1", counting.censuses)
	}

	st, sessions = poolIdentityState(t, config.Agent{Name: "deployer", Dir: "beads", Provider: "test-agent", MaxActiveSessions: intPtr(1)})
	counting = &sessionCensusCountingStore{Store: sessions}
	st.sessionsBeadStore = counting
	if err := st.sp.Start(context.Background(), agentSessionName(st.cityName, "beads/deployer", ""), runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}
	if row := requireAgentRow(t, getAgentList(t, st, "").Items, "beads/deployer"); !row.Running {
		t.Fatalf("beads/deployer running = false, want true (row=%+v)", row)
	}
	if counting.censuses != 0 {
		t.Errorf("session snapshot reads with no missed agent = %d, want 0", counting.censuses)
	}
}

// unreadableSessionStore fails every session snapshot read.
type unreadableSessionStore struct {
	beads.Store
}

func (s unreadableSessionStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == session.BeadType || query.Label == session.LabelSession {
		return nil, errors.New("session store unreachable")
	}
	return s.Store.List(query)
}

// TestAgentListReportsUnreadableSessionSnapshot pins that a missed slot whose
// session cannot be looked up makes the list partial instead of passing the
// slot off as stopped. The rig list says the same about its counts.
func TestAgentListReportsUnreadableSessionSnapshot(t *testing.T) {
	st, sessions := boundedPoolState(t)
	st.sessionsBeadStore = unreadableSessionStore{Store: sessions}

	hasSessionsError := func(errs []string) bool {
		for _, e := range errs {
			if strings.HasPrefix(e, "sessions: ") && strings.Contains(e, "session store unreachable") {
				return true
			}
		}
		return false
	}

	if body := getAgentList(t, st, ""); !body.Partial || !hasSessionsError(body.PartialErrors) {
		t.Errorf("GET /agents = {partial:%v partial_errors:%q}, want partial with the session store error", body.Partial, body.PartialErrors)
	}

	var rigs ListBody[rigResponse]
	rec := httptest.NewRecorder()
	newTestCityHandler(t, st).ServeHTTP(rec, httptest.NewRequest("GET", cityURL(st, "/rigs"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /rigs status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&rigs); err != nil {
		t.Fatalf("decode GET /rigs: %v", err)
	}
	if !rigs.Partial || !hasSessionsError(rigs.PartialErrors) {
		t.Errorf("GET /rigs = {partial:%v partial_errors:%q}, want partial with the session store error", rigs.Partial, rigs.PartialErrors)
	}
}
