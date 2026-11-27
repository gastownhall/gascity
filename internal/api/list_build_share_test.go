package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// heldActiveStore holds every in_progress List call, after it has read the
// store, until it is opened, and counts the calls and the most that ran at
// once. A list build reads a rig store's in_progress beads once
// (activeBeadIndex), so its calls count the builds that reached it, and a
// held build answers with what the store held when it read.
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
	rows, err := s.Store.List(query)
	s.entered <- struct{}{}
	<-s.release
	return rows, err
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
	state *fakeState
	srv   *Server
	h     http.Handler
	url   string
	held  *heldActiveStore
	// want is each row's active bead, by agent name or session id.
	want map[string]string
	// other is url with a query that keys a different list.
	other string
	// inline are urls that build their list inline, sharing no build.
	inline []string
	// malformed is url with a cursor the endpoint refuses, or "".
	malformed string
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
		state:  st,
		srv:    srv,
		h:      newTestCityHandlerWith(t, st, srv),
		url:    cityURL(st, "/agents"),
		held:   held,
		want:   want,
		other:  cityURL(st, "/agents?running=true"),
		inline: []string{cityURL(st, "/agents?peek=true"), cityURL(st, "/agents?index=0&wait=10ms")},
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
		state:     fs,
		srv:       srv,
		h:         newTestCityHandlerWith(t, fs, srv),
		url:       cityURL(fs, "/sessions"),
		held:      held,
		want:      want,
		other:     cityURL(fs, "/sessions?limit=1"),
		inline:    []string{cityURL(fs, "/sessions?peek=true")},
		malformed: cityURL(fs, "/sessions?cursor=not-a-cursor"),
		joined:    func(hook func(string, bool)) { srv.sessionListBuilds.joined = hook },
		slots:     srv.sessionListBuilds.slotCount,
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

// listIndex is the event index a list response reports (X-GC-Index).
func listIndex(t *testing.T, rec *httptest.ResponseRecorder) uint64 {
	t.Helper()
	index, err := strconv.ParseUint(rec.Header().Get("X-GC-Index"), 10, 64)
	if err != nil {
		t.Fatalf("X-GC-Index %q: %v", rec.Header().Get("X-GC-Index"), err)
	}
	return index
}

// TestListBuildSharingPostMutation: a reader that misses while a build runs is
// answered by a build that starts once the running one ends, with the event
// index that build started at, whichever reader queued it. Reader A's build
// holds after it reads the store; reader C queues, creating the next build;
// one row's work then moves to a new bead and the event index advances;
// reader B joins C's queued build. A gets the list as it was, and B and C get
// the changed list, reporting an index no older than the change.
func TestListBuildSharingPostMutation(t *testing.T) {
	for _, fx := range listShareFixtures {
		t.Run(fx.name, func(t *testing.T) {
			f := fx.new(t)
			joins := make(chan bool, 16)
			f.joined(func(_ string, started bool) { joins <- started })
			recs := make(map[string]*httptest.ResponseRecorder, 3)
			var mu sync.Mutex
			var wg sync.WaitGroup
			read := func(reader string) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					rec := f.get(f.url)
					mu.Lock()
					recs[reader] = rec
					mu.Unlock()
				}()
			}

			read("A")
			if started := <-joins; !started {
				t.Fatal("reader A did not start a build")
			}
			<-f.held.entered
			read("C")
			if started := <-joins; started {
				t.Fatal("reader C started a build while one was running")
			}

			row := slices.Sorted(maps.Keys(f.want))[0]
			if err := f.held.Close(f.want[row]); err != nil {
				t.Fatalf("Close(%s): %v", f.want[row], err)
			}
			changed := maps.Clone(f.want)
			changed[row] = addActiveWork(t, f.held.Store, row)
			f.state.eventProv.Record(events.Event{Type: events.BeadUpdated, Actor: "tester", Subject: changed[row]})
			changedIndex, err := f.state.eventProv.LatestSeq()
			if err != nil {
				t.Fatalf("LatestSeq: %v", err)
			}

			read("B")
			if started := <-joins; started {
				t.Fatal("reader B started a build while one was running")
			}
			f.held.open()
			wg.Wait()
			f.srv.waitForBackground()

			if got := listIndex(t, recs["A"]); got >= changedIndex {
				t.Errorf("reader A X-GC-Index = %d, want one before the change at %d", got, changedIndex)
			}
			f.checkRows(t, recs["A"], f.want)
			for _, reader := range []string{"B", "C"} {
				if got := listIndex(t, recs[reader]); got < changedIndex {
					t.Errorf("reader %s X-GC-Index = %d, want at least %d: its build started after the change", reader, got, changedIndex)
				}
				f.checkRows(t, recs[reader], changed)
			}
			if reads, _ := f.held.stats(); reads != 2 {
				t.Errorf("%d builds read the store, want 2", reads)
			}
		})
	}
}

// TestListBuildSharingIsolation: sharing keeps each list's query, config and
// freshness. While one build is held, a reader of another query or of a newer
// config generation starts its own build, a peek or blocking reader builds
// inline beside it, and a malformed cursor is refused at once; none joins the
// held build. Readers of one build each get their own copy of its list.
func TestListBuildSharingIsolation(t *testing.T) {
	for _, fx := range listShareFixtures {
		t.Run(fx.name, func(t *testing.T) {
			f := fx.new(t)
			joins := make(chan bool, 16)
			f.joined(func(_ string, started bool) { joins <- started })
			var mu sync.Mutex
			recs := map[string]*httptest.ResponseRecorder{}
			var wg sync.WaitGroup
			read := func(name, url string) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					rec := f.get(url)
					mu.Lock()
					recs[name] = rec
					mu.Unlock()
				}()
			}
			read("held", f.url)
			if started := <-joins; !started {
				t.Fatal("the first reader did not start a build")
			}
			<-f.held.entered

			read("other query", f.other)
			if started := <-joins; !started {
				t.Error("a reader of another query joined the held build")
			}
			<-f.held.entered
			// The server sees another config, so the list's config
			// generation moves on.
			f.srv.configGeneration(&config.City{})
			read("new config", f.url)
			if started := <-joins; !started {
				t.Error("a reader of a newer config generation joined the held build")
			}
			<-f.held.entered
			for _, url := range f.inline {
				read(url, url)
				<-f.held.entered
			}
			if f.malformed != "" {
				rec := f.get(f.malformed)
				if rec.Code != http.StatusBadRequest {
					t.Errorf("GET malformed cursor while a build ran: status %d, want 400: %s", rec.Code, rec.Body.String())
				}
			}
			if n := len(joins); n != 0 {
				t.Errorf("%d inline or refused readers took a shared build", n)
			}

			f.held.open()
			wg.Wait()
			f.srv.waitForBackground()
			f.checkRows(t, recs["held"], f.want)
			for name, rec := range recs {
				if rec.Code != http.StatusOK {
					t.Errorf("GET %s: status %d: %s", name, rec.Code, rec.Body.String())
				}
			}
			if _, maxRunning := f.held.stats(); maxRunning != 3+len(f.inline) {
				t.Errorf("%d builds ran at once, want %d: one per key and one per inline reader", maxRunning, 3+len(f.inline))
			}
			if n := f.slots(); n != 0 {
				t.Errorf("%d build slots left once the builds ended, want 0", n)
			}
		})
	}

	t.Run("waiters", func(t *testing.T) {
		srv := New(newFakeState(t))
		var sh listBuildSharer[agentResponse]
		joins := make(chan bool, 4)
		sh.joined = func(_ string, started bool) { joins <- started }
		release := make(chan struct{})
		held := func(ctx context.Context) (*ListOutput[agentResponse], error) {
			<-release
			return &ListOutput[agentResponse]{}, ctx.Err()
		}
		list := func() *ListOutput[agentResponse] {
			return &ListOutput[agentResponse]{Index: 7, Body: ListBody[agentResponse]{
				Items:         []agentResponse{{Name: "worker", ActiveBead: "gc-1"}},
				Total:         1,
				Partial:       true,
				PartialErrors: []string{"rig alpha: looking up active work: down"},
			}}
		}
		built := func(ctx context.Context) (*ListOutput[agentResponse], error) { return list(), ctx.Err() }
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sh.share(context.Background(), srv, "k", held); err != nil {
				t.Errorf("share: %v", err)
			}
		}()
		<-joins
		outs := make([]*ListOutput[agentResponse], 2)
		for i := range outs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := sh.share(context.Background(), srv, "k", built)
				if err != nil {
					t.Errorf("share: %v", err)
				}
				outs[i] = out
			}()
			if started := <-joins; started {
				t.Fatal("a waiter started a build while one was running")
			}
		}
		close(release)
		wg.Wait()
		srv.waitForBackground()
		if outs[0] == nil || outs[1] == nil {
			t.Fatal("a waiter got no list")
		}

		outs[0].Index = 99
		outs[0].Body.Items[0].ActiveBead = "mutated"
		outs[0].Body.PartialErrors[0] = "mutated"
		want := list()
		if outs[1].Index != want.Index || outs[1].Body.Items[0].ActiveBead != want.Body.Items[0].ActiveBead || !slices.Equal(outs[1].Body.PartialErrors, want.Body.PartialErrors) {
			t.Errorf("one waiter's changes reached another's list: %+v", *outs[1])
		}
	})
}
