package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// splitReassignFixture is a split city whose sessions and graph classes share
// one binding, with a city work store and the rig work store a test passes in.
// It holds the configured named session "worker", whose only row has lost
// continuity, so the next materializing resolve of "worker" replaces it.
type splitReassignFixture struct {
	fs           *fakeState
	h            http.Handler
	work         beads.Store // the city work store
	binding      beads.Store // the sessions and graph binding
	spec         apiNamedSessionSpec
	historicalID string
}

func newSplitReassignFixture(t *testing.T, rig beads.Store) splitReassignFixture {
	t.Helper()
	fs := newSessionFakeState(t)
	work := splittest.NewWorkStore(t, "gc")
	binding, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = binding.(interface{ CloseStore() error }).CloseStore() })
	fs.cityBeadStore = work
	fs.stores = map[string]beads.Store{"myrig": rig}
	fs.sessionsBeadStore = binding
	fs.graphBeadStore = binding
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	fs.cfg.Agents[0].Dir = ""
	fs.cfg.NamedSessions[0].Dir = ""
	spec, ok, err := srv.findNamedSessionSpecForTarget(binding, "worker")
	if err != nil || !ok {
		t.Fatalf("findNamedSessionSpecForTarget(worker) = %v, %v", ok, err)
	}
	historicalID, err := srv.materializeNamedSession(binding, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession(worker): %v", err)
	}
	if err := binding.SetMetadataBatch(historicalID, map[string]string{"state": "archived", "continuity_eligible": "false"}); err != nil {
		t.Fatal(err)
	}
	return splitReassignFixture{fs: fs, h: h, work: work, binding: binding, spec: spec, historicalID: historicalID}
}

// wake posts a wake of "worker" to the API.
func (f splitReassignFixture) wake() *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, newPostRequest(cityURL(f.fs, "/session/worker/wake"), nil))
	return rec
}

// wakeOK wakes "worker", requires a 200, and returns the woken session's id.
func (f splitReassignFixture) wakeOK(t *testing.T) string {
	t.Helper()
	rec := f.wake()
	if rec.Code != http.StatusOK {
		t.Fatalf("wake = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding the wake response %s: %v", rec.Body.String(), err)
	}
	return resp.ID
}

// TestSplitNamedSessionWakeReassignsWorkInEveryWorkStore is write-audit B7: when
// the API replaces a continuity-ineligible named session, the open work still
// assigned to the retired session must move to its successor wherever that work
// lives. The reassign used to list work through the SESSIONS store, which on a
// split city is the binding: only binding-resident graph steps moved, and every
// task in the city and rig work stores kept the retired session id. Named
// session work is not pool-routed, so nothing else rescues it.
func TestSplitNamedSessionWakeReassignsWorkInEveryWorkStore(t *testing.T) {
	rig := splittest.NewWorkStore(t, "mr")
	f := newSplitReassignFixture(t, rig)
	cityTask := mustCreateAssigned(t, f.work, "city task", f.historicalID)
	rigTask := mustCreateAssigned(t, rig, "rig task", f.historicalID)
	graphStep := mustCreateAssigned(t, f.binding, "graph step", f.historicalID)

	freshID := f.wakeOK(t)
	if freshID == "" || freshID == f.historicalID {
		t.Fatalf("fresh id = %q, want a new canonical bead distinct from %s", freshID, f.historicalID)
	}
	requireAssignee(t, f.work, cityTask.ID, freshID, "city work store")
	requireAssignee(t, rig, rigTask.ID, freshID, "rig work store")
	requireAssignee(t, f.binding, graphStep.ID, freshID, "graph binding")
}

// TestSplitNamedSessionWakeHandsOffPastAFailingWorkStore is write-audit B7's
// failure path. A work store that cannot be read (a rig store that failed to
// open answers every read with an error until the controller reopens its
// stores) must not keep the retired session's other state from moving, nor
// keep its own share from moving for good, nor fail the wake: the wake that
// replaces the session has created and started the successor, so it answers
// 200 with it and logs the store's error (#8360 review). The binding's graph
// step, the wait and the external-message binding still move to the
// successor. A wake that finds the successor canonical while the store still
// fails answers 200 too; the first one after the store reads again moves the
// work it holds.
func TestSplitNamedSessionWakeHandsOffPastAFailingWorkStore(t *testing.T) {
	rig := &failingBeadStore{Store: splittest.NewWorkStore(t, "mr"), listErr: errors.New("rig store unavailable")}
	f := newSplitReassignFixture(t, rig)
	cityTask := mustCreateAssigned(t, f.work, "city task", f.historicalID)
	rigTask := mustCreateAssigned(t, rig, "rig task", f.historicalID)
	graphStep := mustCreateAssigned(t, f.binding, "graph step", f.historicalID)
	wait, err := f.binding.Create(beads.Bead{
		Title:  "historical named wait",
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + f.historicalID},
		Metadata: map[string]string{
			"session_id": f.historicalID,
			"state":      "open",
		},
	})
	if err != nil {
		t.Fatalf("Create(wait): %v", err)
	}
	bound, err := extmsg.NewServices(f.binding).Bindings.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.BindInput{
		Conversation: extmsg.ConversationRef{
			ScopeID:        "test-city",
			Provider:       "discord",
			AccountID:      "acct",
			ConversationID: "thread-historical",
			Kind:           extmsg.ConversationThread,
		},
		SessionID: f.historicalID,
	})
	if err != nil {
		t.Fatalf("Bind(historical): %v", err)
	}

	var logs bytes.Buffer
	restoreLog := captureLog(t, &logs)
	freshID := f.wakeOK(t)
	restoreLog()
	if !strings.Contains(logs.String(), "rig store unavailable") {
		t.Fatalf("the first wake logged %q, want the rig store's error: the hand-off it could not finish must be reported", logs.String())
	}
	successor, ok, err := session.FindCanonicalConfiguredNamedSessionBead(f.binding, f.spec)
	if err != nil || !ok || successor.ID != freshID || freshID == f.historicalID {
		t.Fatalf("successor after the first wake = %q, %v, %v; want the new canonical bead the wake answered (%s)", successor.ID, ok, err, freshID)
	}
	requireAssignee(t, f.work, cityTask.ID, freshID, "city task")
	requireAssignee(t, f.binding, graphStep.ID, freshID, "binding graph step after the rig store failed")
	requireAssignee(t, rig, rigTask.ID, f.historicalID, "rig task while its store cannot be read")
	// Read the rows as stored: resolving a conversation follows a retired
	// session to its successor by name whether or not the binding moved.
	for _, row := range []struct{ id, what string }{
		{wait.ID, "wait"},
		{bound.ID, "external-message binding"},
	} {
		got, err := f.binding.Get(row.id)
		if err != nil {
			t.Fatalf("Get(%s): %v", row.what, err)
		}
		if got.Metadata["session_id"] != freshID {
			t.Errorf("%s %s session_id = %q, want the successor %s after the rig store failed", row.what, row.id, got.Metadata["session_id"], freshID)
		}
	}

	if id := f.wakeOK(t); id != freshID {
		t.Fatalf("wake while the rig store still fails = %q, want the successor %s", id, freshID)
	}
	requireAssignee(t, rig, rigTask.ID, f.historicalID, "rig task while its store still cannot be read")

	rig.listErr = nil
	if id := f.wakeOK(t); id != freshID {
		t.Fatalf("wake after the rig store recovered = %q, want the successor %s", id, freshID)
	}
	requireAssignee(t, rig, rigTask.ID, freshID, "rig task once its store reads again")
}

// TestNamedSessionWakeReRunsTheHandOffOnlyWhereARetiredRowHoldsWork: a wake
// that finds the successor canonical hands it what the identity's retired rows
// still hold (here a rig task assigned to the retired id after the
// replacement), but reads a work store live only where a cached read shows a
// retired row holds work, so once the hand-off is done a wake pays no live
// work-store read for it.
func TestNamedSessionWakeReRunsTheHandOffOnlyWhereARetiredRowHoldsWork(t *testing.T) {
	rig := &listRecordingStore{Store: splittest.NewWorkStore(t, "mr")}
	f := newSplitReassignFixture(t, rig)
	freshID := f.wakeOK(t)
	if freshID == "" || freshID == f.historicalID {
		t.Fatalf("fresh id = %q, want a new canonical bead distinct from %s", freshID, f.historicalID)
	}
	late := mustCreateAssigned(t, rig, "late rig task", f.historicalID)

	if id := f.wakeOK(t); id != freshID {
		t.Fatalf("second wake = %q, want the successor %s", id, freshID)
	}
	requireAssignee(t, rig, late.ID, freshID, "rig task assigned to the retired id after the replacement")

	rig.take(f.historicalID)
	if id := f.wakeOK(t); id != freshID {
		t.Fatalf("third wake = %q, want the successor %s", id, freshID)
	}
	if _, live := rig.take(f.historicalID); live != 0 {
		t.Fatalf("a wake read the rig store live %d times for a retired row that holds nothing there", live)
	}
}

// TestNamedSessionWakeReadsNoStoreLiveForParkedWork: work bd parks (blocked,
// deferred) reads as "open" through a cache but is left out of the live
// "open" read the hand-off moves work by, so it stays on the retired id (the
// parity ga-7uf6c7 tracks), and a retired row holding only parked work costs
// a wake no live read: that read would move nothing.
func TestNamedSessionWakeReadsNoStoreLiveForParkedWork(t *testing.T) {
	rig := &parkedListStore{listRecordingStore: listRecordingStore{Store: splittest.NewWorkStore(t, "mr")}}
	f := newSplitReassignFixture(t, rig)
	parked := mustCreateAssigned(t, rig, "parked rig task", f.historicalID)
	freshID := f.wakeOK(t)
	requireAssignee(t, rig, parked.ID, f.historicalID, "parked rig task after the replacement")

	rig.take(f.historicalID)
	if id := f.wakeOK(t); id != freshID {
		t.Fatalf("second wake = %q, want the successor %s", id, freshID)
	}
	if _, live := rig.take(f.historicalID); live != 0 {
		t.Fatalf("a wake read the rig store live %d times for a retired row that holds only parked work there", live)
	}
}

// parkedListStore answers as a bd-backed store whose every row is parked work:
// a cached read folds the row into "open" and keeps bd's status in
// SourceStatus, and a live "open" read, bd's raw open, leaves the row out.
type parkedListStore struct {
	listRecordingStore
}

func (s *parkedListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.listRecordingStore.List(query)
	if query.Live && query.Status == "open" {
		return nil, err
	}
	for i := range rows {
		rows[i].SourceStatus = "blocked"
	}
	return rows, err
}

// TestSplitNamedSessionWakeMovesASuspendedRigsWorkOnceItResumes: the hand-off
// leaves a rig suspended by effective state untouched, as status and the
// active-bead lookup do (any read of its store restarts its retired proxy and
// Dolt), and the first wake after the rig resumes moves the work it holds.
func TestSplitNamedSessionWakeMovesASuspendedRigsWorkOnceItResumes(t *testing.T) {
	rig := &listRecordingStore{Store: splittest.NewWorkStore(t, "mr")}
	f := newSplitReassignFixture(t, rig)
	rigTask := mustCreateAssigned(t, rig, "rig task", f.historicalID)
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, f.fs.cityPath, "myrig", &suspended); err != nil {
		t.Fatalf("SetRigSuspended(myrig): %v", err)
	}

	freshID := f.wakeOK(t)
	if reads, _ := rig.take(f.historicalID); reads != 0 {
		t.Fatalf("the hand-off read the suspended rig's store %d times", reads)
	}
	requireAssignee(t, rig, rigTask.ID, f.historicalID, "rig task while its rig is suspended")

	suspended = false
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, f.fs.cityPath, "myrig", &suspended); err != nil {
		t.Fatalf("SetRigSuspended(myrig): %v", err)
	}
	if id := f.wakeOK(t); id != freshID {
		t.Fatalf("wake after the rig resumed = %q, want the successor %s", id, freshID)
	}
	requireAssignee(t, rig, rigTask.ID, freshID, "rig task once its rig resumes")
}

func mustCreateAssigned(t *testing.T, store beads.Store, title, assignee string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: title, Type: "task", Assignee: assignee})
	if err != nil {
		t.Fatalf("Create(%s): %v", title, err)
	}
	return b
}

// requireAssignee reports an error unless bead id in store is assigned to want.
func requireAssignee(t *testing.T, store beads.Store, id, want, what string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Assignee != want {
		t.Errorf("%s %s assignee = %q, want %q", what, id, got.Assignee, want)
	}
}

// listRecordingStore records the List queries a store answers.
type listRecordingStore struct {
	beads.Store
	mu      sync.Mutex
	queries []beads.ListQuery
}

func (s *listRecordingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.queries = append(s.queries, query)
	s.mu.Unlock()
	return s.Store.List(query)
}

// take reports how many recorded reads asked for assignee's work and how many
// of those bypassed a cache, and forgets every recorded read.
func (s *listRecordingStore) take(assignee string) (reads, live int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queries {
		if q.Assignee != assignee {
			continue
		}
		reads++
		if q.Live {
			live++
		}
	}
	s.queries = nil
	return reads, live
}

// TestRetiredSessionWorkStoresIsTheSessionsStoreAloneOnASingleStoreCity: a
// city that relocates nothing sweeps exactly the one store it always swept,
// rigs or no rigs.
func TestRetiredSessionWorkStoresIsTheSessionsStoreAloneOnASingleStoreCity(t *testing.T) {
	fs := newSessionFakeState(t)
	city := fs.cityBeadStore
	fs.stores = map[string]beads.Store{"myrig": beads.NewMemStore()}
	if got := New(fs).retiredSessionWorkStores(city); len(got) != 1 || got[0] != city {
		t.Fatalf("single-store sweep = %v, want only the sessions store", got)
	}
}

// TestRetiredSessionWorkStoresSkipsRigsSuspendedByEffectiveState: a split
// city's sweep leaves a rig's store untouched exactly when status and the
// active-bead lookup do (cacheColdRigNames): the rig is suspended at runtime
// (`gc rig suspend`), by suspended_on_start, or by the legacy suspended field,
// and no runtime resume overrides it. A store a suspended rig shares with a
// rig that is not suspended is swept through that rig.
func TestRetiredSessionWorkStoresSkipsRigsSuspendedByEffectiveState(t *testing.T) {
	suspend, resume := true, false
	for _, tc := range []struct {
		name    string
		rig     config.Rig
		runtime *bool // the rig's runtime override, as gc rig suspend/resume records it
		swept   bool
	}{
		{name: "running", rig: config.Rig{Name: "myrig"}, swept: true},
		{name: "suspended at runtime", rig: config.Rig{Name: "myrig"}, runtime: &suspend},
		{name: "suspended on start", rig: config.Rig{Name: "myrig", SuspendedOnStart: true}},
		{name: "suspended on start, resumed at runtime", rig: config.Rig{Name: "myrig", SuspendedOnStart: true}, runtime: &resume, swept: true},
		{name: "legacy suspended", rig: config.Rig{Name: "myrig", Suspended: true}},
		{name: "legacy suspended, resumed at runtime", rig: config.Rig{Name: "myrig", Suspended: true}, runtime: &resume, swept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			rig := beads.NewMemStore()
			binding := beads.NewMemStore()
			tc.rig.Path = "/tmp/myrig"
			fs.cfg.Rigs = []config.Rig{tc.rig}
			fs.stores = map[string]beads.Store{"myrig": rig}
			fs.sessionsBeadStore = binding
			fs.graphBeadStore = binding
			if tc.runtime != nil {
				if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, fs.cityPath, "myrig", tc.runtime); err != nil {
					t.Fatalf("SetRigSuspended(myrig): %v", err)
				}
			}
			got := New(fs).retiredSessionWorkStores(binding)
			if swept := slices.Contains(got, beads.Store(rig)); swept != tc.swept {
				t.Fatalf("rig store swept = %t, want %t (sweep = %v)", swept, tc.swept, got)
			}
			if !slices.Contains(got, fs.cityBeadStore) || !slices.Contains(got, beads.Store(binding)) {
				t.Fatalf("split sweep = %v, want the city store and the binding", got)
			}
		})
	}

	t.Run("store shared with a rig that is not suspended", func(t *testing.T) {
		fs := newSessionFakeState(t)
		shared := beads.NewMemStore()
		binding := beads.NewMemStore()
		fs.cfg.Rigs = []config.Rig{{Name: "alpha", Path: "/tmp/alpha"}, {Name: "beta", Path: "/tmp/beta"}}
		fs.stores = map[string]beads.Store{"alpha": shared, "beta": shared}
		fs.sessionsBeadStore = binding
		fs.graphBeadStore = binding
		if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, fs.cityPath, "alpha", &suspend); err != nil {
			t.Fatalf("SetRigSuspended(alpha): %v", err)
		}
		if got := New(fs).retiredSessionWorkStores(binding); !slices.Contains(got, beads.Store(shared)) {
			t.Fatalf("split sweep = %v, want the store alpha shares with beta", got)
		}
	})
}
