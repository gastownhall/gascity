package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// activeReadCountingStore counts the in_progress List calls made through it,
// whatever their assignee filter, and is safe to read once the request that
// made them has answered.
type activeReadCountingStore struct {
	beads.Store
	mu    sync.Mutex
	reads int
}

func (s *activeReadCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status == "in_progress" {
		s.mu.Lock()
		s.reads++
		s.mu.Unlock()
	}
	return s.Store.List(query)
}

func (s *activeReadCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// runningWorkers configures n running agents worker1..workerN in rig myrig,
// each with a session id, and returns their qualified names and session ids.
func runningWorkers(t *testing.T, st *fakeState, n int) (names, sessionIDs []string) {
	t.Helper()
	st.cfg.NamedSessions = nil
	st.cfg.Agents = nil
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("worker%d", i)
		st.cfg.Agents = append(st.cfg.Agents, config.Agent{Name: name, Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)})
		sessionName := "myrig--" + name
		if err := st.sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
			t.Fatalf("Start(%s): %v", sessionName, err)
		}
		id := fmt.Sprintf("mc-session-%d", i)
		if err := st.sp.SetMeta(sessionName, "GC_SESSION_ID", id); err != nil {
			t.Fatalf("SetMeta(%s, GC_SESSION_ID): %v", sessionName, err)
		}
		names = append(names, "myrig/"+name)
		sessionIDs = append(sessionIDs, id)
	}
	return names, sessionIDs
}

// TestAgentListReadsEachStoreOnce: GET /agents resolves every row's active
// bead from one in_progress read of the rig's store, whatever the number of
// rows and of identities each row asks for (session id, runtime name,
// qualified name), and still names each row's own bead. Through a store
// cache the read is the cache's when it serves one: a primed cache reads its
// backing store not at all, and a cache that declines reads it once.
func TestAgentListReadsEachStoreOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cached      bool
		primed      bool
		wantBacking int
	}{
		{name: "uncached store", wantBacking: 1},
		{name: "cache that declines", cached: true, wantBacking: 1},
		{name: "primed cache", cached: true, primed: true, wantBacking: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeState(t)
			names, sessionIDs := runningWorkers(t, st, 6)
			backing := &activeReadCountingStore{Store: beads.NewMemStore()}
			want := make(map[string]string, len(names))
			for i, name := range names {
				// Odd rows hold work under their session id, even rows
				// under their qualified name, the fallback identity.
				owner := name
				if i%2 == 1 {
					owner = sessionIDs[i]
				}
				want[name] = addActiveWork(t, backing.Store, owner)
			}
			var store beads.Store = backing
			if tc.cached {
				cache := beads.NewCachingStoreForTest(backing, nil)
				if tc.primed {
					if err := cache.Prime(context.Background()); err != nil {
						t.Fatalf("Prime: %v", err)
					}
				}
				store = cache
			}
			st.stores["myrig"] = store
			before := backing.count()

			items := getAgentList(t, st, "").Items

			if len(items) != len(names) {
				t.Fatalf("GET /agents rows = %v, want %v", agentNames(items), names)
			}
			for _, item := range items {
				if item.ActiveBead != want[item.Name] {
					t.Errorf("%s active_bead = %q, want %q", item.Name, item.ActiveBead, want[item.Name])
				}
			}
			if got := backing.count() - before; got > tc.wantBacking {
				t.Errorf("GET /agents over %d rows made %d in_progress reads of the backing store, want at most %d", len(names), got, tc.wantBacking)
			}
		})
	}
}

// TestSessionListReadsEachStoreOnce: both session-list handlers resolve a
// page's active beads with one in_progress read of each rig store the page
// reaches, however many rows and identities (session id, runtime name,
// alias, template) ask, and still name each row's own bead from its own rig.
func TestSessionListReadsEachStoreOnce(t *testing.T) {
	for _, handler := range []string{"huma", "legacy"} {
		t.Run(handler, func(t *testing.T) {
			fs := newSessionFakeState(t)
			fs.cfg.Rigs = append(fs.cfg.Rigs,
				config.Rig{Name: "alpha", Path: "/tmp/alpha"},
				config.Rig{Name: "beta", Path: "/tmp/beta"},
			)
			alpha := &activeReadCountingStore{Store: &beads.MemStore{IDPrefix: "alpha"}}
			beta := &activeReadCountingStore{Store: &beads.MemStore{IDPrefix: "beta"}}
			fs.stores["alpha"] = alpha
			fs.stores["beta"] = beta
			want := map[string]string{}
			for i := range 6 {
				info := createTestSession(t, fs.cityBeadStore, fs.sp, fmt.Sprintf("session %d", i))
				store := alpha.Store
				if i >= 3 {
					store = beta.Store
				}
				want[info.ID] = addActiveWork(t, store, info.ID)
			}
			srv := New(fs)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
			if handler == "legacy" {
				req = httptest.NewRequest("GET", "/v0/sessions", nil)
				srv.handleSessionList(rec, req)
			} else {
				newTestCityHandlerWith(t, fs, srv).ServeHTTP(rec, req)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /sessions status = %d: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Items []sessionResponse `json:"items"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Items) != len(want) {
				t.Fatalf("GET /sessions listed %d sessions, want %d", len(resp.Items), len(want))
			}
			for _, item := range resp.Items {
				if item.ActiveBead != want[item.ID] {
					t.Errorf("session %s active_bead = %q, want %q", item.ID, item.ActiveBead, want[item.ID])
				}
			}
			for name, store := range map[string]*activeReadCountingStore{"alpha": alpha, "beta": beta} {
				if n := store.count(); n != 1 {
					t.Errorf("rig %s: %d in_progress reads for a page of %d sessions, want 1", name, n, len(want))
				}
			}
		})
	}
}
