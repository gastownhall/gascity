package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// fixedOrderStore answers in_progress List calls with its rows in the order
// given, filtered by the query's assignees, as a backend that orders rows of
// equal creation time its own way would.
type fixedOrderStore struct {
	beads.Store
	rows []beads.Bead
}

func (s *fixedOrderStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status != "in_progress" {
		return s.Store.List(query)
	}
	var out []beads.Bead
	for _, b := range s.rows {
		if len(query.Assignees) == 0 || slices.Contains(query.Assignees, b.Assignee) {
			out = append(out, b)
		}
	}
	return out, nil
}

// TestActiveBeadIndexPrecedenceAndScope: one index answers every row of a
// list build exactly as the live lookup answers that row alone: identity order
// first, then store order, then newest first within a store (backend order
// for equal creation times), exact trimmed assignee matches, blank and
// repeated identities dropped, and each row's effective scope (a suspended
// rig's own store never read, a store it shares with an active rig read
// through that rig, an unknown rig searching every store). Rows sharing a
// store read it once, and a store no row reaches is not read at all.
func TestActiveBeadIndexPrecedenceAndScope(t *testing.T) {
	type row struct {
		rig        string
		identities []string
		want       string // fixture key of the expected bead, "" for none
	}
	tieTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		rows      []row
		wantReads map[string]int // per counted store; absent means 0
	}{
		{
			name:      "identity order beats store order",
			rows:      []row{{"", []string{"alias-2", "sess-1"}, "beta alias-2"}},
			wantReads: map[string]int{"alpha": 1, "beta": 1},
		},
		{
			name:      "first store wins for one identity, newest within it",
			rows:      []row{{"", []string{"sess-1", "tmpl"}, "alpha sess-1 newer"}},
			wantReads: map[string]int{"alpha": 1},
		},
		{
			name: "backend order decides equal creation times",
			rows: []row{{"ties", []string{"tie"}, "ties first"}},
			// fixedOrderStore is not counted: it is the only store read.
		},
		{
			name: "assignees match exactly after trimming",
			rows: []row{
				{"", []string{"work"}, ""},
				{"", []string{"WORKER"}, ""},
				{"", []string{" worker "}, "alpha worker"},
			},
			wantReads: map[string]int{"alpha": 1, "beta": 1, "zeta": 1},
		},
		{
			name: "blank and repeated identities are dropped",
			rows: []row{
				{"", []string{"", "  ", "alias-2", "alias-2", "sess-1"}, "beta alias-2"},
				{"", []string{"", "  "}, ""},
			},
			wantReads: map[string]int{"alpha": 1, "beta": 1},
		},
		{
			name: "rows share one read per store",
			rows: []row{
				{"", []string{"sess-1"}, "alpha sess-1 newer"},
				{"", []string{"tmpl"}, "alpha tmpl"},
				{"", []string{"alias-2"}, "beta alias-2"},
				{"beta", []string{"sess-1"}, "beta sess-1"},
				{"gamma", []string{"alias-2"}, "beta alias-2"},
				{"zeta", []string{"tmpl"}, "zeta tmpl"},
			},
			wantReads: map[string]int{"alpha": 1, "beta": 1, "zeta": 1},
		},
		{
			name:      "rig scope reads only that rig's store",
			rows:      []row{{"zeta", []string{"sess-1", "tmpl"}, "zeta tmpl"}},
			wantReads: map[string]int{"zeta": 1},
		},
		{
			name: "suspended rig's own store is never read",
			rows: []row{
				{"cold", []string{"sess-4"}, ""},
				{"", []string{"sess-4"}, ""},
			},
			wantReads: map[string]int{"alpha": 1, "beta": 1, "zeta": 1},
		},
		{
			name:      "suspended rig reads the store it shares with an active rig",
			rows:      []row{{"chill", []string{"alias-2", "sess-1"}, "beta alias-2"}},
			wantReads: map[string]int{"beta": 1},
		},
		{
			name:      "unknown rig searches every store",
			rows:      []row{{"nosuch", []string{"sess-5"}, "zeta sess-5"}},
			wantReads: map[string]int{"alpha": 1, "beta": 1, "zeta": 1},
		},
		{
			name: "empty page reads nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeState(t)
			st.cfg.Rigs = []config.Rig{
				{Name: "alpha", Path: "/tmp/alpha"},
				{Name: "beta", Path: "/tmp/beta"},
				{Name: "chill", Path: "/tmp/chill", SuspendedOnStart: true},
				{Name: "cold", Path: "/tmp/cold", SuspendedOnStart: true},
				{Name: "gamma", Path: "/tmp/gamma"},
				{Name: "ties", Path: "/tmp/ties"},
				{Name: "zeta", Path: "/tmp/zeta"},
			}
			counted := map[string]*listCountingStore{
				"alpha": {Store: &beads.MemStore{IDPrefix: "alpha"}},
				"beta":  {Store: &beads.MemStore{IDPrefix: "beta"}},
				"cold":  {Store: &beads.MemStore{IDPrefix: "cold"}},
				"zeta":  {Store: &beads.MemStore{IDPrefix: "zeta"}},
			}
			ids := map[string]string{
				"alpha sess-1 older": addActiveWork(t, counted["alpha"].Store, "sess-1"),
				"alpha sess-1 newer": addActiveWork(t, counted["alpha"].Store, "sess-1"),
				"alpha tmpl":         addActiveWork(t, counted["alpha"].Store, "tmpl"),
				"alpha worker":       addActiveWork(t, counted["alpha"].Store, "worker"),
				"beta sess-1":        addActiveWork(t, counted["beta"].Store, "sess-1"),
				"beta alias-2":       addActiveWork(t, counted["beta"].Store, "alias-2"),
				"cold sess-4":        addActiveWork(t, counted["cold"].Store, "sess-4"),
				"zeta sess-5":        addActiveWork(t, counted["zeta"].Store, "sess-5"),
				"zeta tmpl":          addActiveWork(t, counted["zeta"].Store, "tmpl"),
				"ties first":         "ties-2",
			}
			ties := &fixedOrderStore{Store: beads.NewMemStore(), rows: []beads.Bead{
				{ID: "ties-2", Assignee: "tie", Status: "in_progress", CreatedAt: tieTime},
				{ID: "ties-1", Assignee: "tie", Status: "in_progress", CreatedAt: tieTime},
			}}
			st.stores = map[string]beads.Store{
				"alpha": counted["alpha"],
				"beta":  counted["beta"],
				"chill": counted["beta"],
				"cold":  counted["cold"],
				"gamma": counted["beta"],
				"ties":  ties,
				"zeta":  counted["zeta"],
			}
			srv := New(st)

			index := srv.newActiveBeadIndex()
			got := make([]string, len(tc.rows))
			for i, r := range tc.rows {
				got[i] = index.lookup(r.rig, r.identities...)
			}
			reads := make(map[string]int, len(counted))
			for name, store := range counted {
				reads[name] = store.listCalls
			}

			for i, r := range tc.rows {
				if want := ids[r.want]; got[i] != want {
					t.Errorf("row %d lookup(%q, %q) = %q, want %q (%s)", i, r.rig, r.identities, got[i], want, r.want)
				}
				if live := srv.findLiveActiveBeadForAssignees(r.rig, r.identities...); got[i] != live {
					t.Errorf("row %d lookup(%q, %q) = %q, live lookup = %q", i, r.rig, r.identities, got[i], live)
				}
			}
			for name, n := range reads {
				if n != tc.wantReads[name] {
					t.Errorf("store %s: %d reads for the build, want %d", name, n, tc.wantReads[name])
				}
			}
		})
	}
}

// activeReadFailingStore fails its in_progress List calls with err, keeping
// the rows that decoded when err is a partial result, and counts them.
type activeReadFailingStore struct {
	beads.Store
	mu    sync.Mutex
	err   error
	reads int
}

func (s *activeReadFailingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status != "in_progress" {
		return s.Store.List(query)
	}
	s.mu.Lock()
	s.reads++
	err := s.err
	s.mu.Unlock()
	rows, listErr := s.Store.List(query)
	switch {
	case listErr != nil:
		return nil, listErr
	case err == nil:
		return rows, nil
	case beads.IsPartialResult(err):
		return rows, err
	default:
		return nil, err
	}
}

func (s *activeReadFailingStore) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *activeReadFailingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// TestActiveBeadIndexPartialErrors: an active-work read that fails is logged
// once, whatever the number of rows reaching the store, and the store is not
// read again within the build. A failed read answers no row, so a row falls
// through to the next store in scope; a partial result still answers from the
// rows that decoded. The agent list reports the failure once in its
// partial_errors and is not cached: the next request, once the store reads
// again, rebuilds and answers in full. A session list does not report it: its
// partial flag says the roster is incomplete, and the roster is whole.
func TestActiveBeadIndexPartialErrors(t *testing.T) {
	failed := errors.New("bd list: exit status 1")
	partial := &beads.PartialResultError{Op: "bd list", Err: errors.New("decoding an unrelated bead")}

	for _, tc := range []struct {
		name      string
		err       error
		keepsRows bool
	}{
		{name: "failed read", err: failed},
		{name: "partial read", err: partial, keepsRows: true},
	} {
		t.Run("agent list/"+tc.name, func(t *testing.T) {
			st := newFakeState(t)
			names, _ := runningWorkers(t, st, 4)
			store := &activeReadFailingStore{Store: beads.NewMemStore(), err: tc.err}
			want := make(map[string]string, len(names))
			for _, name := range names {
				want[name] = addActiveWork(t, store.Store, name)
			}
			st.stores["myrig"] = store
			var logs bytes.Buffer
			t.Cleanup(captureLog(t, &logs))
			h := newTestCityHandlerWith(t, st, New(st))
			list := func() agentListWire {
				t.Helper()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(st, "/agents"), nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET /agents status = %d: %s", rec.Code, rec.Body.String())
				}
				var resp agentListWire
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				return resp
			}

			first := list()

			for _, item := range first.Items {
				wantBead := ""
				if tc.keepsRows {
					wantBead = want[item.Name]
				}
				if item.ActiveBead != wantBead {
					t.Errorf("%s active_bead = %q, want %q", item.Name, item.ActiveBead, wantBead)
				}
			}
			wantErrs := []string{fmt.Sprintf("rig myrig: looking up active work: %v", tc.err)}
			if !first.Partial || !slices.Equal(first.PartialErrors, wantErrs) {
				t.Errorf("partial = %v, partial_errors = %q, want true and %q", first.Partial, first.PartialErrors, wantErrs)
			}
			if n := store.count(); n != 1 {
				t.Errorf("failing store read %d times for %d rows, want 1", n, len(names))
			}
			if n := strings.Count(logs.String(), `looking up active work in rig "myrig"`); n != 1 {
				t.Errorf("failed read logged %d times, want 1; log:\n%s", n, logs.String())
			}

			store.setErr(nil)
			second := list()

			if second.Partial || store.count() != 2 {
				t.Errorf("after the store recovered: partial = %v (%q), store reads = %d; want a full rebuild, not the cached partial list", second.Partial, second.PartialErrors, store.count())
			}
			for _, item := range second.Items {
				if item.ActiveBead != want[item.Name] {
					t.Errorf("after recovery %s active_bead = %q, want %q", item.Name, item.ActiveBead, want[item.Name])
				}
			}
		})
	}

	for _, handler := range []string{"huma", "legacy"} {
		t.Run("session list/"+handler, func(t *testing.T) {
			fs := newSessionFakeState(t)
			fs.cfg.Rigs = append(fs.cfg.Rigs,
				config.Rig{Name: "alpha", Path: "/tmp/alpha"},
				config.Rig{Name: "beta", Path: "/tmp/beta"},
			)
			alpha := &activeReadFailingStore{Store: &beads.MemStore{IDPrefix: "alpha"}, err: failed}
			beta := &beads.MemStore{IDPrefix: "beta"}
			fs.stores["alpha"] = alpha
			fs.stores["beta"] = beta
			want := map[string]string{}
			for i := range 4 {
				info := createTestSession(t, fs.cityBeadStore, fs.sp, fmt.Sprintf("session %d", i))
				// alpha sorts first, so its bead would win had it read.
				addActiveWork(t, alpha.Store, info.ID)
				want[info.ID] = addActiveWork(t, beta, info.ID)
			}
			srv := New(fs)
			var logs bytes.Buffer
			t.Cleanup(captureLog(t, &logs))

			rec := httptest.NewRecorder()
			if handler == "legacy" {
				srv.handleSessionList(rec, httptest.NewRequest("GET", "/v0/sessions", nil))
			} else {
				newTestCityHandlerWith(t, fs, srv).ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil))
			}

			if rec.Code != http.StatusOK {
				t.Fatalf("GET /sessions status = %d: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Items         []sessionResponse `json:"items"`
				Partial       bool              `json:"partial"`
				PartialErrors []string          `json:"partial_errors"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Items) != len(want) {
				t.Fatalf("GET /sessions listed %d sessions, want %d", len(resp.Items), len(want))
			}
			for _, item := range resp.Items {
				if item.ActiveBead != want[item.ID] {
					t.Errorf("session %s active_bead = %q, want beta's %q", item.ID, item.ActiveBead, want[item.ID])
				}
			}
			if resp.Partial || len(resp.PartialErrors) != 0 {
				t.Errorf("partial = %v, partial_errors = %q; want a whole roster, not a partial one", resp.Partial, resp.PartialErrors)
			}
			if n := alpha.count(); n != 1 {
				t.Errorf("failing store read %d times for %d sessions, want 1", n, len(want))
			}
			if n := strings.Count(logs.String(), `looking up active work in rig "alpha"`); n != 1 {
				t.Errorf("failed read logged %d times, want 1; log:\n%s", n, logs.String())
			}
		})
	}
}
