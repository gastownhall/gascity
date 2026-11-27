package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// heldActiveStore holds every in_progress List call until it is opened, and
// counts the calls and the most that ran at once. A list build reads a rig
// store's in_progress beads once (activeBeadIndex), so its calls count the
// builds that reached it.
type heldActiveStore struct {
	beads.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu         sync.Mutex
	reads      int
	running    int
	maxRunning int
}

func newHeldActiveStore(store beads.Store) *heldActiveStore {
	return &heldActiveStore{Store: store, entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (s *heldActiveStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status != "in_progress" {
		return s.Store.List(query)
	}
	s.mu.Lock()
	s.reads++
	s.running++
	s.maxRunning = max(s.maxRunning, s.running)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}()
	s.entered <- struct{}{}
	<-s.release
	return s.Store.List(query)
}

// open lets every held call, and every later one, through.
func (s *heldActiveStore) open() {
	s.once.Do(func() { close(s.release) })
}

func (s *heldActiveStore) stats() (reads, maxRunning int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, s.maxRunning
}

// slotCount is the number of keys with a build running.
func (sh *listBuildSharer[T]) slotCount() int {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return len(sh.slots)
}

// listShareFixture is a city whose list endpoint reads its active work from
// held, with work assigned to every row.
type listShareFixture struct {
	srv  *Server
	h    http.Handler
	url  string
	held *heldActiveStore
	// want is each row's active bead, by agent name or session id.
	want map[string]string
	// joined sets the endpoint's sharer hook.
	joined func(func(key string, started bool))
	// slots counts the endpoint's keys with a build running.
	slots func() int
	// rows decodes a list response into each row's active bead.
	rows func(t *testing.T, rec *httptest.ResponseRecorder) map[string]string
}

// listShareFixtures builds a fixture for each list endpoint that shares its
// builds.
var listShareFixtures = []struct {
	name string
	new  func(t *testing.T) *listShareFixture
}{
	{name: "agents", new: newAgentListShareFixture},
	{name: "sessions", new: newSessionListShareFixture},
}

func newAgentListShareFixture(t *testing.T) *listShareFixture {
	t.Helper()
	st := newFakeState(t)
	names, _ := runningWorkers(t, st, 3)
	held := newHeldActiveStore(beads.NewMemStore())
	want := make(map[string]string, len(names))
	for _, name := range names {
		want[name] = addActiveWork(t, held.Store, name)
	}
	st.stores["myrig"] = held
	srv := New(st)
	f := &listShareFixture{
		srv:    srv,
		h:      newTestCityHandlerWith(t, st, srv),
		url:    cityURL(st, "/agents"),
		held:   held,
		want:   want,
		joined: func(hook func(string, bool)) { srv.agentListBuilds.joined = hook },
		slots:  srv.agentListBuilds.slotCount,
		rows: func(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
			t.Helper()
			var resp agentListWire
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			rows := make(map[string]string, len(resp.Items))
			for _, item := range resp.Items {
				rows[item.Name] = item.ActiveBead
			}
			return rows
		},
	}
	t.Cleanup(held.open)
	return f
}

func newSessionListShareFixture(t *testing.T) *listShareFixture {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: "alpha", Path: "/tmp/alpha"})
	held := newHeldActiveStore(&beads.MemStore{IDPrefix: "alpha"})
	fs.stores["alpha"] = held
	want := map[string]string{}
	for i := range 3 {
		info := createTestSession(t, fs.cityBeadStore, fs.sp, fmt.Sprintf("session %d", i))
		want[info.ID] = addActiveWork(t, held.Store, info.ID)
	}
	srv := New(fs)
	f := &listShareFixture{
		srv:    srv,
		h:      newTestCityHandlerWith(t, fs, srv),
		url:    cityURL(fs, "/sessions"),
		held:   held,
		want:   want,
		joined: func(hook func(string, bool)) { srv.sessionListBuilds.joined = hook },
		slots:  srv.sessionListBuilds.slotCount,
		rows: func(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
			t.Helper()
			var resp struct {
				Items []sessionResponse `json:"items"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			rows := make(map[string]string, len(resp.Items))
			for _, item := range resp.Items {
				rows[item.ID] = item.ActiveBead
			}
			return rows
		},
	}
	t.Cleanup(held.open)
	return f
}

// get serves one GET of url through f's handler.
func (f *listShareFixture) get(url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	return rec
}

// checkRows fails unless rec is a 200 list naming every row's own bead.
func (f *listShareFixture) checkRows(t *testing.T, rec *httptest.ResponseRecorder, want map[string]string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
	}
	got := f.rows(t, rec)
	if len(got) != len(want) {
		t.Fatalf("listed rows %v, want %v", got, want)
	}
	for row, bead := range want {
		if got[row] != bead {
			t.Errorf("row %s active_bead = %q, want %q", row, got[row], bead)
		}
	}
}

// TestListBuildSharingQueuedFresh: while one build of a list runs, held in
// its store, seven identical readers that miss queue behind it on one build
// instead of starting seven. When the held build ends, the queued build runs,
// after it and not beside it, and answers all seven; then the key's slot is
// gone.
func TestListBuildSharingQueuedFresh(t *testing.T) {
	for _, fx := range listShareFixtures {
		t.Run(fx.name, func(t *testing.T) {
			f := fx.new(t)
			joins := make(chan bool, 16)
			f.joined(func(_ string, started bool) { joins <- started })
			const queued = 7
			recs := make([]*httptest.ResponseRecorder, queued+1)
			var wg sync.WaitGroup
			read := func(i int) {
				defer wg.Done()
				recs[i] = f.get(f.url)
			}

			wg.Add(1)
			go read(0)
			if started := <-joins; !started {
				t.Fatal("the first reader did not start a build")
			}
			<-f.held.entered
			wg.Add(queued)
			for i := 1; i <= queued; i++ {
				go read(i)
			}
			for range queued {
				if started := <-joins; started {
					t.Fatal("a reader started a build while one was running")
				}
			}
			f.held.open()
			wg.Wait()
			f.srv.waitForBackground()

			for _, rec := range recs {
				f.checkRows(t, rec, f.want)
			}
			reads, maxRunning := f.held.stats()
			if reads != 2 {
				t.Errorf("%d builds read the store, want 2: the held build and one for every reader queued behind it", reads)
			}
			if maxRunning != 1 {
				t.Errorf("%d builds ran at once, want 1", maxRunning)
			}
			if n := f.slots(); n != 0 {
				t.Errorf("%d build slots left once the builds ended, want 0", n)
			}
		})
	}
}
