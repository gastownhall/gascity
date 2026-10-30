package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

type partialAgentSessionLister struct {
	running []string
	err     error
}

func (p partialAgentSessionLister) ListRunning(prefix string) ([]string, error) {
	var filtered []string
	for _, name := range p.running {
		if len(prefix) == 0 || strings.HasPrefix(name, prefix) {
			filtered = append(filtered, name)
		}
	}
	return filtered, p.err
}

type activeBeadQueryStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *activeBeadQueryStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status == "in_progress" && (query.Assignee != "" || len(query.Assignees) > 0) {
		s.queries = append(s.queries, query)
	}
	return s.Store.List(query)
}

// activeBeadErrStore returns err from every List alongside whatever rows the
// wrapped store matched, standing in for a live read that fails outright or
// decodes only some rows.
type activeBeadErrStore struct {
	beads.Store
	err error
}

func (s activeBeadErrStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	return rows, s.err
}

// addActiveWork creates an in_progress bead assigned to assignee in store and
// returns its ID.
func addActiveWork(t *testing.T, store beads.Store, assignee string) string {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "assigned work"})
	if err != nil {
		t.Fatalf("Create(%s work): %v", assignee, err)
	}
	status := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(%s): %v", b.ID, err)
	}
	return b.ID
}

func TestAgentList(t *testing.T) {
	state := newFakeState(t)
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1", resp.Total)
	}
}

func TestAgentListPoolExpansion(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{
			Name:              "polecat",
			Dir:               "myrig",
			MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 3",
		},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Total != 3 {
		t.Fatalf("Total = %d, want 3", resp.Total)
	}

	// Check pool member names.
	want := []string{"myrig/polecat-1", "myrig/polecat-2", "myrig/polecat-3"}
	for i, name := range want {
		if resp.Items[i].Name != name {
			t.Errorf("Items[%d].Name = %q, want %q", i, resp.Items[i].Name, name)
		}
		if resp.Items[i].Pool != "myrig/polecat" {
			t.Errorf("Items[%d].Pool = %q, want %q", i, resp.Items[i].Pool, "myrig/polecat")
		}
	}
}

func TestAgentListUnlimitedPoolDiscovery(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{
			Name:              "polecat",
			Dir:               "myrig",
			MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1),
		},
	}
	// Start 2 running sessions matching the pool pattern.
	state.sp.Start(context.Background(), "myrig--polecat-1", runtime.Config{}) //nolint:errcheck
	state.sp.Start(context.Background(), "myrig--polecat-2", runtime.Config{}) //nolint:errcheck
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2", resp.Total)
	}

	// Both discovered instances should reference the pool.
	for i, item := range resp.Items {
		if item.Pool != "myrig/polecat" {
			t.Errorf("Items[%d].Pool = %q, want %q", i, item.Pool, "myrig/polecat")
		}
		if !item.Running {
			t.Errorf("Items[%d].Running = false, want true", i)
		}
	}
}

func TestDiscoverUnlimitedPoolFailsClosedOnPartialListResults(t *testing.T) {
	a := config.Agent{
		Name:              "polecat",
		Dir:               "myrig",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(-1),
	}
	sp := partialAgentSessionLister{
		running: []string{"myrig--polecat-1", "myrig--polecat-2"},
		err:     &runtime.PartialListError{Err: errors.New("remote backend down")},
	}

	got := discoverUnlimitedPool(a, "myrig/polecat", "test-city", "", sp)
	if len(got) != 0 {
		t.Fatalf("len = %d, want fail-closed empty result on partial list", len(got))
	}
}

func TestAgentListUnlimitedImportedPoolDiscovery(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{
			Name:              "polecat",
			Dir:               "myrig",
			BindingName:       "gs",
			MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1),
		},
	}
	state.sp.Start(context.Background(), "myrig--gs__polecat-1", runtime.Config{}) //nolint:errcheck
	state.sp.Start(context.Background(), "myrig--gs__polecat-2", runtime.Config{}) //nolint:errcheck
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2", resp.Total)
	}

	for i, item := range resp.Items {
		if item.Name != "myrig/gs.polecat-1" && item.Name != "myrig/gs.polecat-2" {
			t.Errorf("Items[%d].Name = %q, want imported pool member name", i, item.Name)
		}
		if item.Pool != "myrig/gs.polecat" {
			t.Errorf("Items[%d].Pool = %q, want %q", i, item.Pool, "myrig/gs.polecat")
		}
		if !item.Running {
			t.Errorf("Items[%d].Running = false, want true", i)
		}
	}
}

// gc init writes an agent with no max_active_sessions plus a [[named_session]]
// for it. That template is pool-shaped, and instance discovery only matches
// "<name>-" sessions, so the named session has to reach the roster from
// config, running or not. A routed-work instance of the template is live
// here, so the roster is not empty and the configured-pool fallback cannot
// stand in for the named session.
func TestAgentListIncludesOwnNamedSessionOfUnlimitedTemplate(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			state := newFakeState(t)
			initCfg := config.DefaultCity(state.cityName)
			state.cfg.Agents = initCfg.Agents
			state.cfg.NamedSessions = initCfg.NamedSessions
			identity := initCfg.NamedSessions[0].QualifiedName()
			instance := identity + "-1"
			live := []string{instance}
			if running {
				live = append(live, identity)
			}
			for _, qn := range live {
				if err := state.sp.Start(context.Background(), agentSessionName(state.cityName, qn, ""), runtime.Config{}); err != nil {
					t.Fatalf("start session for %s: %v", qn, err)
				}
			}
			srv := New(state)
			h := newTestCityHandlerWith(t, state, srv)

			req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			var resp struct {
				Items []agentResponse `json:"items"`
				Total int             `json:"total"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Total != 2 {
				t.Fatalf("Total = %d, want 2: named session %s and instance %s (items=%+v)", resp.Total, identity, instance, resp.Items)
			}
			byName := make(map[string]agentResponse, len(resp.Items))
			for _, item := range resp.Items {
				byName[item.Name] = item
			}
			item, ok := byName[identity]
			if !ok {
				t.Fatalf("no row for named session %s (items=%+v)", identity, resp.Items)
			}
			if item.Running != running {
				t.Errorf("%s Running = %v, want %v", identity, item.Running, running)
			}
			if item.Pool != identity {
				t.Errorf("%s Pool = %q, want %q, matching GET /agent/%s", identity, item.Pool, identity, identity)
			}
			if inst, ok := byName[instance]; !ok || !inst.Running {
				t.Errorf("instance %s = %+v (listed=%v), want a running row", instance, inst, ok)
			}
		})
	}
}

// A failed session listing leaves the template's instances unknown, but its
// own named session comes from config: the roster keeps that row and names
// the failure in partial_errors rather than dropping the agent.
func TestAgentListKeepsOwnNamedSessionWhenListingFails(t *testing.T) {
	state := newFakeState(t)
	state.sp = runtime.NewFailFake()
	initCfg := config.DefaultCity(state.cityName)
	state.cfg.Agents = initCfg.Agents
	state.cfg.NamedSessions = initCfg.NamedSessions
	identity := initCfg.NamedSessions[0].QualifiedName()
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp struct {
		Items         []agentResponse `json:"items"`
		PartialErrors []string        `json:"partial_errors"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.ContainsFunc(resp.PartialErrors, func(e string) bool { return strings.HasPrefix(e, "agent "+identity+": ") }) {
		t.Fatalf("partial_errors = %v, want an entry naming agent %s", resp.PartialErrors, identity)
	}
	if !slices.ContainsFunc(resp.Items, func(item agentResponse) bool { return item.Name == identity }) {
		t.Fatalf("no row for named session %s beside the listing error (items=%+v)", identity, resp.Items)
	}
}

func TestFindAgentUnlimitedPoolMember(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "polecat",
				Dir:               "myrig",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1),
			},
		},
	}
	// Unlimited pool members follow the pattern {name}-{N}.
	a, ok := findAgent(cfg, "myrig/polecat-5")
	if !ok {
		t.Fatal("findAgent(myrig/polecat-5) = false, want true for unlimited pool")
	}
	if a.Name != "polecat" {
		t.Errorf("agent.Name = %q, want %q", a.Name, "polecat")
	}

	// Non-numeric suffix should not match.
	_, ok = findAgent(cfg, "myrig/polecat-abc")
	if ok {
		t.Error("findAgent(myrig/polecat-abc) = true, want false for non-numeric suffix")
	}
}

func TestFindAgentCanonicalSingletonPoolRejectsSuffix(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
		},
	}
	if a, ok := findAgent(cfg, "myrig/worker"); !ok || a.QualifiedName() != "myrig/worker" {
		t.Fatalf("findAgent(myrig/worker) = (%q, %v), want canonical template", a.QualifiedName(), ok)
	}
	if _, ok := findAgent(cfg, "myrig/worker-1"); ok {
		t.Fatal("findAgent(myrig/worker-1) = true, want false for canonical singleton pool")
	}
	expanded := expandAgent(cfg.Agents[0], "city", "", nil, nil)
	if len(expanded) != 1 {
		t.Fatalf("expandAgent() returned %d entries, want 1", len(expanded))
	}
	if expanded[0].qualifiedName != "myrig/worker" {
		t.Fatalf("expandAgent()[0].qualifiedName = %q, want myrig/worker", expanded[0].qualifiedName)
	}
}

func TestExpandAgentDisabledAgentUsesConfiguredIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(0)},
		},
	}
	if isMultiSessionAgent(cfg.Agents[0]) {
		t.Fatal("isMultiSessionAgent(max=0) = true, want false")
	}
	expanded := expandAgent(cfg.Agents[0], "city", "", nil, nil)
	if len(expanded) != 1 {
		t.Fatalf("expandAgent() returned %d entries, want 1", len(expanded))
	}
	if expanded[0].qualifiedName != "myrig/worker" {
		t.Fatalf("expandAgent()[0].qualifiedName = %q, want myrig/worker", expanded[0].qualifiedName)
	}
	if expanded[0].pool != "" {
		t.Fatalf("expandAgent()[0].pool = %q, want empty", expanded[0].pool)
	}
}

func TestExpandAgentListsOwnNamedSessionOfPool(t *testing.T) {
	coordinator := config.Agent{Name: "coordinator"}
	ownNamed := []config.NamedSession{{Template: "coordinator", Mode: "always"}}
	tests := []struct {
		name    string
		agent   config.Agent
		named   []config.NamedSession
		running []string
		noSP    bool
		want    []string
	}{
		{name: "unlimited with nothing running", agent: coordinator, named: ownNamed, want: []string{"coordinator"}},
		{
			name: "unlimited keeps discovered instances", agent: coordinator, named: ownNamed,
			running: []string{"coordinator", "coordinator-gc-7"},
			want:    []string{"coordinator", "coordinator-gc-7"},
		},
		{
			name: "explicit pool settings are left alone", agent: config.Agent{Name: "coordinator", MaxActiveSessions: intPtr(-1)}, named: ownNamed,
			running: []string{"coordinator-gc-7"},
			want:    []string{"coordinator-gc-7"},
		},
		{name: "no session provider lists it once", agent: coordinator, named: ownNamed, noSP: true, want: []string{"coordinator"}},
		{
			name:  "imported rig agent",
			agent: config.Agent{Name: "refinery", Dir: "myrig", BindingName: "gs"},
			named: []config.NamedSession{{Template: "refinery", Dir: "myrig", BindingName: "gs"}},
			want:  []string{"myrig/gs.refinery"},
		},
		{name: "named session under another identity", agent: coordinator, named: []config.NamedSession{{Name: "lead", Template: "coordinator"}}},
		{name: "no named session", agent: coordinator, running: []string{"coordinator-gc-7"}, want: []string{"coordinator-gc-7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sp sessionLister
			if !tt.noSP {
				sp = partialAgentSessionLister{running: tt.running}
			}
			var names []string
			for _, ea := range expandAgent(tt.agent, "city", "", sp, tt.named) {
				names = append(names, ea.qualifiedName)
				if ea.pool != tt.agent.QualifiedName() {
					t.Errorf("%s: pool = %q, want %q", ea.qualifiedName, ea.pool, tt.agent.QualifiedName())
				}
			}
			if !slices.Equal(names, tt.want) {
				t.Fatalf("expandAgent() = %v, want %v", names, tt.want)
			}
		})
	}
}

// TestFindAgentBoundedPoolMember covers bounded multi-session pools — both
// V1 (no BindingName) and V2 (with BindingName), with and without
// NamepoolNames. The V2 cases regressed silently because the bounded
// path enumerated raw member names without applying the binding prefix
// the listing endpoint adds via Agent.QualifiedInstanceName, so detail
// lookups for those instances 404'd while the listing happily emitted
// them. The unlimited path already handled this correctly.
func TestFindAgentBoundedPoolMember(t *testing.T) {
	tests := []struct {
		name     string
		agent    config.Agent
		query    string
		wantOK   bool
		wantName string
	}{
		{
			name: "v1 numeric in rig",
			agent: config.Agent{
				Name:              "polecat",
				Dir:               "myrig",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
			query: "myrig/polecat-1", wantOK: true, wantName: "polecat",
		},
		{
			name: "v1 namepool in rig",
			agent: config.Agent{
				Name:              "polecat",
				Dir:               "myrig",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
				NamepoolNames: []string{"alpha", "bravo", "charlie"},
			},
			query: "myrig/bravo", wantOK: true, wantName: "polecat",
		},
		{
			name: "v2 numeric city-scoped",
			agent: config.Agent{
				Name:              "dog",
				BindingName:       "gastown",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
			query: "gastown.dog-1", wantOK: true, wantName: "dog",
		},
		{
			name: "v2 numeric in rig",
			agent: config.Agent{
				Name:              "polecat",
				Dir:               "myrig",
				BindingName:       "gastown",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
			query: "myrig/gastown.polecat-2", wantOK: true, wantName: "polecat",
		},
		{
			name: "v2 namepool in rig",
			agent: config.Agent{
				Name:              "polecat",
				Dir:               "nextlex-legal-rag",
				BindingName:       "gastown",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5),
				NamepoolNames: []string{"furiosa", "nux", "slit", "rictus", "capable"},
			},
			query: "nextlex-legal-rag/gastown.furiosa", wantOK: true, wantName: "polecat",
		},
		{
			name: "v2 namepool city-scoped",
			agent: config.Agent{
				Name:              "polecat",
				BindingName:       "gastown",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
				NamepoolNames: []string{"furiosa", "nux"},
			},
			query: "gastown.nux", wantOK: true, wantName: "polecat",
		},
		{
			name: "v2 unknown instance does not match",
			agent: config.Agent{
				Name:              "dog",
				BindingName:       "gastown",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
			query: "gastown.dog-99", wantOK: false,
		},
		{
			name: "v2 namepool unknown member does not match",
			agent: config.Agent{
				Name:              "polecat",
				BindingName:       "gastown",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
				NamepoolNames: []string{"furiosa", "nux"},
			},
			query: "gastown.unknown", wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.City{Agents: []config.Agent{tt.agent}}
			a, ok := findAgent(cfg, tt.query)
			if ok != tt.wantOK {
				t.Fatalf("findAgent(%q) ok = %v, want %v", tt.query, ok, tt.wantOK)
			}
			if tt.wantOK && a.Name != tt.wantName {
				t.Errorf("findAgent(%q).Name = %q, want %q", tt.query, a.Name, tt.wantName)
			}
		})
	}
}

func TestAgentListFilterByRig(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "rig1", MaxActiveSessions: intPtr(1)},
		{Name: "worker", Dir: "rig2", MaxActiveSessions: intPtr(1)},
	}
	state.cfg.Rigs = []config.Rig{
		{Name: "rig1", Path: filepath.Join(state.cityPath, "repos", "rig1")},
		{Name: "rig2", Path: filepath.Join(state.cityPath, "repos", "rig2")},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents?rig=rig1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1", resp.Total)
	}
	if resp.Items[0].Name != "rig1/worker" {
		t.Errorf("Name = %q, want %q", resp.Items[0].Name, "rig1/worker")
	}
}

func TestAgentListFilterByRunning(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{Name: "running-agent", MaxActiveSessions: intPtr(1)},
		{Name: "stopped-agent", MaxActiveSessions: intPtr(1)},
	}
	state.sp.Start(context.Background(), "running-agent", runtime.Config{}) //nolint:errcheck
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents?running=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1", resp.Total)
	}
	if resp.Items[0].Name != "running-agent" {
		t.Errorf("Name = %q, want %q", resp.Items[0].Name, "running-agent")
	}
}

// TestAgentListSuspendedCheckGatedByRunning guards against a regression
// where the suspended-flag GetMeta lookup ran unconditionally, even for
// agents with no session at all. A non-running agent has no session to
// query, so the handler must not issue any GetMeta call for it.
func TestAgentListSuspendedCheckGatedByRunning(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if n := state.sp.CountCalls("GetMeta", "myrig--worker"); n != 0 {
		t.Errorf("GetMeta calls for non-running agent = %d, want 0 (suspended check must be gated behind running)", n)
	}
}

// rosterFake augments runtime.Fake with the optional batch-read interfaces
// ([runtime.SessionRosterProvider], [runtime.EnvironmentBatchProvider]) so
// tests can assert the handler prefers them over per-session-name calls.
type rosterFake struct {
	*runtime.Fake
	roster map[string]runtime.SessionRosterEntry
	env    map[string]map[string]string
}

func (r *rosterFake) SessionRoster() (map[string]runtime.SessionRosterEntry, error) {
	return r.roster, nil
}

func (r *rosterFake) GetAllEnvironment(name string) (map[string]string, error) {
	return r.env[name], nil
}

// TestAgentListPrefersBatchSessionRosterAndEnvironment verifies that when
// the session provider implements the optional batch-read interfaces, the
// handler sources running/attached/last-activity/suspended/session-id from
// the single batched calls instead of falling back to the per-session-name
// IsRunning/IsAttached/GetLastActivity/GetMeta path.
func TestAgentListPrefersBatchSessionRosterAndEnvironment(t *testing.T) {
	state := newFakeState(t)
	lastActivity := time.Unix(1700000000, 0)
	fake := &rosterFake{
		Fake: runtime.NewFake(),
		roster: map[string]runtime.SessionRosterEntry{
			"myrig--worker": {Attached: true, LastActivity: lastActivity},
		},
		env: map[string]map[string]string{
			"myrig--worker": {"suspended": "true", "GC_SESSION_ID": "sess-xyz"},
		},
	}
	// Liveness still comes from IsRunning, so the session must actually be
	// started; the roster only supplies attributes for it.
	fake.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
	state.sessionProvider = fake
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("Total = %d, want 1", resp.Total)
	}

	item := resp.Items[0]
	if !item.Running {
		t.Error("Running = false, want true")
	}
	if !item.Suspended {
		t.Error("Suspended = false, want true (from batched GetAllEnvironment)")
	}
	if item.Session == nil || !item.Session.Attached {
		t.Errorf("Session.Attached = %+v, want Attached=true (from roster)", item.Session)
	}
	if item.Session == nil || item.Session.LastActivity == nil || !item.Session.LastActivity.Equal(lastActivity) {
		t.Errorf("Session.LastActivity = %+v, want %v (from roster)", item.Session, lastActivity)
	}

	// IsRunning is deliberately absent: it is the liveness source and is
	// already a single cached fleet-wide read, so batching does not replace
	// it. The reads below are the ones the batch path subsumes.
	for _, method := range []string{"IsAttached", "GetLastActivity", "GetMeta"} {
		if n := fake.CountCalls(method, "myrig--worker"); n != 0 {
			t.Errorf("%s calls = %d, want 0 (batch path should bypass per-session-name methods)", method, n)
		}
	}
}

// TestAgentListRosterPresenceIsNotLiveness guards the invariant that the
// batched session roster is an attributes source, not a liveness source.
// The runtime's session listing still reports a session whose pane has
// exited (tmux keeps such a corpse listed under remain-on-exit), while
// IsRunning excludes it. A name present in the roster but not running must
// therefore report running=false, or a crashed agent surfaces as idle.
func TestAgentListRosterPresenceIsNotLiveness(t *testing.T) {
	state := newFakeState(t)
	fake := &rosterFake{
		Fake: runtime.NewFake(),
		roster: map[string]runtime.SessionRosterEntry{
			// Present in the roster, but never started: the corpse case.
			"myrig--worker": {Attached: true, LastActivity: time.Unix(1700000000, 0)},
		},
		env: map[string]map[string]string{},
	}
	state.sessionProvider = fake
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("Total = %d, want 1", resp.Total)
	}

	item := resp.Items[0]
	if item.Running {
		t.Error("Running = true, want false (roster presence must not imply liveness)")
	}
	if item.Session != nil {
		t.Errorf("Session = %+v, want nil for a non-running agent", item.Session)
	}
}

func TestAgentGet(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp agentResponse
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Name != "myrig/worker" {
		t.Errorf("Name = %q, want %q", resp.Name, "myrig/worker")
	}
}

func TestAgentGetActiveBeadUsesSessionIDOwnership(t *testing.T) {
	state := newFakeState(t)
	sessionName := "myrig--worker"
	sessionID := "mc-session"
	if err := state.sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start(%s): %v", sessionName, err)
	}
	if err := state.sp.SetMeta(sessionName, "GC_SESSION_ID", sessionID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	work, err := state.stores["myrig"].Create(beads.Bead{
		Title: "active work",
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := sessionID
	if err := state.stores["myrig"].Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp agentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.ActiveBead; got != work.ID {
		t.Fatalf("active_bead = %q, want %q", got, work.ID)
	}
}

func TestAgentListActiveBeadUsesCachedLookup(t *testing.T) {
	state := newFakeState(t)
	sessionName := "myrig--worker"
	sessionID := "mc-session"
	if err := state.sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start(%s): %v", sessionName, err)
	}
	if err := state.sp.SetMeta(sessionName, "GC_SESSION_ID", sessionID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	store := &activeBeadQueryStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	work, err := store.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := sessionID
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(resp.Items))
	}
	if got := resp.Items[0].ActiveBead; got != work.ID {
		t.Fatalf("active_bead = %q, want %q", got, work.ID)
	}
	if len(store.queries) != 1 {
		t.Fatalf("active-bead queries = %d, want 1", len(store.queries))
	}
	if store.queries[0].Live {
		t.Fatal("agent list active-bead lookup should stay cached")
	}
}

func TestAgentGetActiveBeadUsesLiveLookup(t *testing.T) {
	state := newFakeState(t)
	sessionName := "myrig--worker"
	sessionID := "mc-session"
	if err := state.sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start(%s): %v", sessionName, err)
	}
	if err := state.sp.SetMeta(sessionName, "GC_SESSION_ID", sessionID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := sessionID
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache

	reassigned := "other-session"
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &reassigned}); err != nil {
		t.Fatalf("reassign backing work: %v", err)
	}

	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp agentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.ActiveBead; got != "" {
		t.Fatalf("active_bead = %q, want empty after external reassignment", got)
	}
}

func TestAgentGetNotFound(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/nonexistent"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAgentOutputPeekFallback(t *testing.T) {
	state := newFakeState(t)
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
	state.sp.SetPeekOutput("myrig--worker", "Hello from agent")
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp agentOutputResponse
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Format != "text" {
		t.Errorf("format = %q, want %q", resp.Format, "text")
	}
	if resp.Agent != "myrig/worker" {
		t.Errorf("agent = %q, want %q", resp.Agent, "myrig/worker")
	}
	if len(resp.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(resp.Turns))
	}
	if resp.Turns[0].Text != "Hello from agent" {
		t.Errorf("text = %q, want %q", resp.Turns[0].Text, "Hello from agent")
	}
	if resp.Turns[0].Role != "output" {
		t.Errorf("role = %q, want %q", resp.Turns[0].Role, "output")
	}
}

func TestFindAgentPoolMaxZero(t *testing.T) {
	// Regression: pool with Max=0 should default to 1, matching expandAgent.
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "polecat",
				Dir:               "myrig",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(0), ScaleCheck: "echo 0",
			},
		},
	}
	// Max=0 defaults to 1 member, so "polecat" (no suffix) should be found.
	a, ok := findAgent(cfg, "myrig/polecat")
	if !ok {
		t.Fatal("findAgent(myrig/polecat) = false, want true for pool with Max=0")
	}
	if a.Name != "polecat" {
		t.Errorf("agent.Name = %q, want %q", a.Name, "polecat")
	}
}

func TestFindAgent_QualifiedGenericRigScopedTemplate(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "reviewer", Scope: "rig"},
		},
		Rigs: []config.Rig{
			{Name: "alpha"},
			{Name: "beta"},
		},
	}

	a, ok := findAgent(cfg, "alpha/reviewer")
	if !ok {
		t.Fatal("findAgent(alpha/reviewer) = false, want true")
	}
	if got := a.QualifiedName(); got != "alpha/reviewer" {
		t.Fatalf("QualifiedName() = %q, want alpha/reviewer", got)
	}
}

func TestAgentOutputNotRunning(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAgentSuspendResume(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	// Suspend.
	req := newPostRequest(cityURL(state, "/agent/myrig/worker/suspend"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("suspend: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !state.suspended["myrig/worker"] {
		t.Error("agent not suspended")
	}

	// Resume.
	req = newPostRequest(cityURL(state, "/agent/myrig/worker/resume"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("resume: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if state.suspended["myrig/worker"] {
		t.Error("agent still suspended after resume")
	}
}

func TestAgentRuntimeActionsRemoved(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	// Unknown actions (kill/drain/undrain/nudge/restart) are rejected
	// by the spec's action enum at the Huma validation layer, before
	// the handler runs. A 422 with a Problem Details body is the
	// contract for "your request violated the input schema."
	for _, action := range []string{"kill", "drain", "undrain", "nudge", "restart"} {
		req := newPostRequest(cityURL(state, "/agent/myrig/worker/")+action, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want %d", action, rec.Code, http.StatusUnprocessableEntity)
		}
	}
}

func TestAgentActionNotFound(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/agent/nonexistent/suspend"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAgentActionNotMutator(t *testing.T) {
	// fakeState (not fakeMutatorState) doesn't implement StateMutator.
	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(state, "/agent/myrig/worker/suspend"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
}

func TestAgentProviderAndDisplayName(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Workspace.Provider = "claude"
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", Provider: "claude", MaxActiveSessions: intPtr(1)},
		{Name: "coder", Dir: "myrig", MaxActiveSessions: intPtr(1)},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []agentResponse `json:"items"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck

	if len(resp.Items) < 2 {
		t.Fatalf("expected at least 2 agents, got %d", len(resp.Items))
	}

	// First agent has explicit provider.
	if resp.Items[0].Provider != "claude" {
		t.Errorf("Items[0].Provider = %q, want %q", resp.Items[0].Provider, "claude")
	}
	if resp.Items[0].DisplayName != "Claude Code" {
		t.Errorf("Items[0].DisplayName = %q, want %q", resp.Items[0].DisplayName, "Claude Code")
	}

	// Second agent inherits workspace default.
	if resp.Items[1].Provider != "claude" {
		t.Errorf("Items[1].Provider = %q, want %q", resp.Items[1].Provider, "claude")
	}
}

func TestAgentStateEnum(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*fakeState)
		wantState string
	}{
		{
			name:      "stopped",
			setup:     func(_ *fakeState) {},
			wantState: "stopped",
		},
		{
			name: "idle",
			setup: func(s *fakeState) {
				s.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
			},
			wantState: "idle",
		},
		{
			name: "suspended",
			setup: func(s *fakeState) {
				s.cfg.Agents = []config.Agent{
					{Name: "worker", Dir: "myrig", Suspended: true, MaxActiveSessions: intPtr(1)},
				}
			},
			wantState: "suspended",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newFakeState(t)
			tt.setup(state)
			srv := New(state)
			h := newTestCityHandlerWith(t, state, srv)

			req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker"), nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			var resp agentResponse
			json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
			if resp.State != tt.wantState {
				t.Errorf("State = %q, want %q", resp.State, tt.wantState)
			}
		})
	}
}

func TestAgentPeekViaQueryParam(t *testing.T) {
	state := newFakeState(t)
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
	state.sp.SetPeekOutput("myrig--worker", "line1\nline2\nline3")
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	// Without ?peek=true — no last_output.
	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []agentResponse `json:"items"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Items[0].LastOutput != "" {
		t.Error("expected empty last_output without ?peek=true")
	}

	// With ?peek=true — includes last_output.
	req = httptest.NewRequest("GET", cityURL(state, "/agents?peek=true"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Items[0].LastOutput == "" {
		t.Error("expected non-empty last_output with ?peek=true")
	}
}

func TestAgentModelAndContext(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Workspace.Provider = "claude"
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", Provider: "claude", MaxActiveSessions: intPtr(1)},
	}
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: "/tmp/myrig"}}
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck

	// Create a fake session JSONL file for the rig path.
	searchDir := t.TempDir()
	slug := sessionlog.ProjectSlug("/tmp/myrig")
	slugDir := filepath.Join(searchDir, slug)
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write session JSONL with model + usage.
	sessionFile := filepath.Join(slugDir, "test-session.jsonl")
	lines := `{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-5-20251101","usage":{"input_tokens":10000,"cache_read_input_tokens":5000,"cache_creation_input_tokens":2000}}}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := New(state)
	srv.sessionLogSearchPaths = []string{searchDir}
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp agentResponse
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Model != "claude-opus-4-5-20251101" {
		t.Errorf("Model = %q, want %q", resp.Model, "claude-opus-4-5-20251101")
	}
	if resp.ContextPct == nil {
		t.Error("expected non-nil ContextPct")
	} else if *resp.ContextPct != 8 {
		t.Errorf("ContextPct = %d, want 8", *resp.ContextPct)
	}
	if resp.ContextWindow == nil {
		t.Error("expected non-nil ContextWindow")
	} else if *resp.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000", *resp.ContextWindow)
	}
}

func TestAgentActivityFromSessionLog(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Workspace.Provider = "claude"
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", Provider: "claude", MaxActiveSessions: intPtr(1)},
	}
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: "/tmp/myrig"}}
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck

	searchDir := t.TempDir()
	slug := sessionlog.ProjectSlug("/tmp/myrig")
	slugDir := filepath.Join(searchDir, slug)
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write session JSONL ending with tool_use stop_reason → "in-turn".
	sessionFile := filepath.Join(slugDir, "test-session.jsonl")
	lines := `{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-5-20251101","stop_reason":"tool_use","content":[{"type":"tool_use"}],"usage":{"input_tokens":10000}}}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := New(state)
	srv.sessionLogSearchPaths = []string{searchDir}
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp agentResponse
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Activity != "in-turn" {
		t.Errorf("Activity = %q, want %q", resp.Activity, "in-turn")
	}
}

func TestResolveProviderInfo(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Providers: map[string]config.ProviderSpec{
			"custom": {DisplayName: "My Custom Agent"},
		},
	}

	tests := []struct {
		agentProvider   string
		wantProvider    string
		wantDisplayName string
	}{
		{"claude", "claude", "Claude Code"},
		{"", "claude", "Claude Code"},           // falls back to workspace
		{"custom", "custom", "My Custom Agent"}, // city-level override
		{"unknown", "unknown", "Unknown"},       // title-cased fallback
	}

	for _, tt := range tests {
		t.Run(tt.agentProvider, func(t *testing.T) {
			provider, displayName := resolveProviderInfo(tt.agentProvider, cfg)
			if provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", provider, tt.wantProvider)
			}
			if displayName != tt.wantDisplayName {
				t.Errorf("displayName = %q, want %q", displayName, tt.wantDisplayName)
			}
		})
	}
}

func TestComputeAgentState(t *testing.T) {
	now := func() *time.Time { t := time.Now(); return &t }()
	old := func() *time.Time { t := time.Now().Add(-20 * time.Minute); return &t }()

	tests := []struct {
		name        string
		suspended   bool
		quarantined bool
		running     bool
		activeBead  string
		lastAct     *time.Time
		want        string
	}{
		{"suspended", true, false, true, "", nil, "suspended"},
		{"quarantined", false, true, false, "", nil, "quarantined"},
		{"stopped", false, false, false, "", nil, "stopped"},
		{"idle", false, false, true, "", nil, "idle"},
		{"working", false, false, true, "bead-1", now, "working"},
		{"waiting", false, false, true, "bead-1", old, "waiting"},
		{"working-no-activity", false, false, true, "bead-1", nil, "waiting"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeAgentState(tt.suspended, tt.quarantined, tt.running, tt.activeBead, tt.lastAct)
			if got != tt.want {
				t.Errorf("computeAgentState() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAgentList_BaseOnlyDescendantUsesResolvedCache covers the
// base-only descendant contract: a [providers.codex-max] declared
// with `base = "builtin:codex"` and no explicit command must still
// report `display_name` + `available=true` in /v0/agents, because
// the resolved-provider cache carries the inherited Command and the
// display name comes from the builtin ancestor.
func TestAgentList_BaseOnlyDescendantUsesResolvedCache(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		// MaxActiveSessions=1 keeps this a non-pool agent so expansion
		// yields a single entry — SupportsInstanceExpansion returns
		// true when the field is unset (unlimited pool by default).
		{Name: "mayor", Dir: "myrig", Provider: "codex-max", MaxActiveSessions: intPtr(1)},
	}
	baseCodex := "builtin:codex"
	state.cfg.Providers = map[string]config.ProviderSpec{
		// Base-only descendant: no Command, no DisplayName.
		"codex-max": {Base: &baseCodex},
	}
	state.cfg.ResolvedProviders = map[string]config.ResolvedProvider{
		"codex-max": {
			Name:            "codex-max",
			BuiltinAncestor: "codex",
			Command:         "codex", // inherited from builtin:codex
			Chain: []config.HopIdentity{
				{Kind: "custom", Name: "codex-max"},
				{Kind: "builtin", Name: "codex"},
			},
		},
	}
	srv := New(state)
	// Simulate the binary being installed by overriding LookPath.
	srv.LookPathFunc = func(bin string) (string, error) {
		if bin == "codex" {
			return "/usr/local/bin/codex", nil
		}
		return "", os.ErrNotExist
	}
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp struct {
		Items []agentResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items length = %d, want 1", len(resp.Items))
	}
	agent := resp.Items[0]
	if agent.Provider != "codex-max" {
		t.Errorf("provider = %q, want codex-max", agent.Provider)
	}
	// DisplayName must come from the builtin ancestor (Codex CLI),
	// since the leaf provider did not declare one.
	if agent.DisplayName != "Codex CLI" {
		t.Errorf("display_name = %q, want %q (inherited from builtin:codex)", agent.DisplayName, "Codex CLI")
	}
	// The binary "codex" is stubbed as available, so the agent must
	// be reported available.
	if !agent.Available {
		t.Errorf("available = false (reason: %q); want true — resolved cache should surface inherited command", agent.UnavailableReason)
	}
}

// TestProviderPathCheck_BaseOnlyDescendant ensures the PATH probe uses
// the inherited command from the resolved cache rather than the empty
// Command on the raw spec.
func TestProviderPathCheck_BaseOnlyDescendant(t *testing.T) {
	baseCodex := "builtin:codex"
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{
			"codex-max": {Base: &baseCodex},
		},
		ResolvedProviders: map[string]config.ResolvedProvider{
			"codex-max": {
				Name:            "codex-max",
				BuiltinAncestor: "codex",
				Command:         "codex",
			},
		},
	}
	got := providerPathCheck("codex-max", cfg)
	if got != "codex" {
		t.Errorf("providerPathCheck = %q, want %q", got, "codex")
	}
}

// TestProviderPathCheck_FallsBackToRawWhenNoCache keeps Phase A configs
// working: when the resolved cache is empty, we still read raw
// Command/PathCheck for the provider.
func TestProviderPathCheck_FallsBackToRawWhenNoCache(t *testing.T) {
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{
			"custom": {Command: "custom-cli"},
		},
	}
	if got := providerPathCheck("custom", cfg); got != "custom-cli" {
		t.Errorf("providerPathCheck = %q, want custom-cli", got)
	}
}

// TestProviderPathCheck_StripsCommandArgs mirrors the config-side
// pathCheckBinary behavior: an unset PathCheck with an args-bearing
// Command must resolve to the bare executable token, so PATH detection
// checks "my-agent" rather than the whole "my-agent --agent coder" string.
func TestProviderPathCheck_StripsCommandArgs(t *testing.T) {
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{
			"custom": {Command: "my-agent --agent coder"},
		},
	}
	if got := providerPathCheck("custom", cfg); got != "my-agent" {
		t.Errorf("providerPathCheck = %q, want my-agent", got)
	}
}

// TestWaitForAgentVisibilityIn_ReturnsImmediatelyOnHit covers the happy
// path: the freshly created agent is already visible in the snapshot
// and the wait returns without sleeping.
func TestWaitForAgentVisibilityIn_ReturnsImmediatelyOnHit(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", Dir: "alpha"}},
	}
	calls := 0
	snapshot := func() *config.City {
		calls++
		return cfg
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := WaitForAgentVisibilityIn(ctx, snapshot, "alpha/worker"); err != nil {
		t.Fatalf("WaitForAgentVisibilityIn: %v", err)
	}
	if calls != 1 {
		t.Errorf("snapshot calls = %d, want 1 (no polling on hit)", calls)
	}
}

// TestWaitForAgentVisibilityIn_PollsUntilVisible covers the race recovery
// path: a stale runtime tick clobbers the snapshot after CreateAgent, the
// next runtime tick restores it, and the wait succeeds once the agent
// reappears.
func TestWaitForAgentVisibilityIn_PollsUntilVisible(t *testing.T) {
	stale := &config.City{}
	fresh := &config.City{
		Agents: []config.Agent{{Name: "worker", Dir: "alpha"}},
	}
	calls := 0
	snapshot := func() *config.City {
		calls++
		if calls < 3 {
			return stale
		}
		return fresh
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForAgentVisibilityIn(ctx, snapshot, "alpha/worker", time.Millisecond); err != nil {
		t.Fatalf("waitForAgentVisibilityIn: %v", err)
	}
	if calls < 3 {
		t.Errorf("snapshot calls = %d, want >= 3 (polled past stale snapshots)", calls)
	}
}

// TestWaitForAgentVisibilityIn_RespectsContext covers the bounded-failure
// case: the agent never appears and the wait surfaces ctx.Err() instead of
// blocking indefinitely.
func TestWaitForAgentVisibilityIn_RespectsContext(t *testing.T) {
	snapshot := func() *config.City { return &config.City{} }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := waitForAgentVisibilityIn(ctx, snapshot, "alpha/worker", time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want wrapped DeadlineExceeded", err)
	}
}

// TestAgentListProvenanceCityNative verifies that an agent declared inline in
// city.toml (present in both raw and expanded config) reports pack_derived=false
// with an empty pack binding. A UI uses this to route an edit to the direct
// PATCH /agent/{name} route rather than the patch overlay.
func TestAgentListProvenanceCityNative(t *testing.T) {
	state := newFakeState(t)
	// Default fake: "worker" in "myrig" is inline (raw == expanded), so it
	// is city-native, not pack-derived.
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("Total = %d, want 1", resp.Total)
	}
	if resp.Items[0].PackDerived {
		t.Errorf("PackDerived = true, want false for city-native agent")
	}
	if resp.Items[0].Pack != "" {
		t.Errorf("Pack = %q, want empty for city-native agent", resp.Items[0].Pack)
	}
}

// TestAgentListProvenancePackDerived verifies that an agent originating from an
// imported pack (present in expanded config but absent from raw) reports
// pack_derived=true and pack = its import binding name. A UI uses this to route
// an edit through the patch overlay (PUT /patches/agents) instead of attempting
// a direct mutation that would return ErrPackDerived / 409.
func TestAgentListProvenancePackDerived(t *testing.T) {
	state := newFakeState(t)
	// Expanded agent carries the import binding name that brought it into
	// scope. Mirrors config-explain's TestHandleConfigExplain_PackDerivedAgent:
	// pack-derived means present in expanded (cfg) but absent from raw.
	state.cfg.Agents = []config.Agent{
		{
			Name:              "worker",
			Dir:               "myrig",
			Provider:          "test-agent",
			BindingName:       "gastown",
			MaxActiveSessions: intPtr(1),
		},
	}
	state.cfg.NamedSessions = nil
	state.rawCfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		// No agents in raw — worker comes from pack expansion.
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/tmp/myrig"},
		},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("Total = %d, want 1", resp.Total)
	}
	if !resp.Items[0].PackDerived {
		t.Errorf("PackDerived = false, want true for pack-derived agent")
	}
	if resp.Items[0].Pack != "gastown" {
		t.Errorf("Pack = %q, want %q (the import binding name)", resp.Items[0].Pack, "gastown")
	}
}

// TestAgentGetProvenanceCityNative verifies the single-agent GET carries the
// same provenance as the list: a city-native agent reports pack_derived=false
// with an empty pack. (The non-omitempty pack_derived bool would otherwise
// read as a confident false on this surface.)
func TestAgentGetProvenanceCityNative(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp agentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PackDerived {
		t.Errorf("PackDerived = true, want false for city-native agent")
	}
	if resp.Pack != "" {
		t.Errorf("Pack = %q, want empty for city-native agent", resp.Pack)
	}
}

// TestAgentGetProvenancePackDerived verifies the single-agent GET reports
// pack_derived=true and the import binding name for a pack-derived agent,
// matching the list surface so a UI gets the same routing signal either way.
func TestAgentGetProvenancePackDerived(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{
			Name:              "worker",
			Dir:               "myrig",
			Provider:          "test-agent",
			BindingName:       "gastown",
			MaxActiveSessions: intPtr(1),
		},
	}
	state.cfg.NamedSessions = nil
	state.rawCfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/tmp/myrig"},
		},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	// Binding-stamped agents are addressed by their qualified name
	// (dir/binding.base), matching AgentMatchesIdentity.
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/gastown.worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp agentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.PackDerived {
		t.Errorf("PackDerived = false, want true for pack-derived agent")
	}
	if resp.Pack != "gastown" {
		t.Errorf("Pack = %q, want %q (the import binding name)", resp.Pack, "gastown")
	}
}

// TestAgentListProvenancePoolInheritance verifies that every instance of a
// bounded multi-session (pool) agent inherits the declared agent's provenance.
// Provenance is a property of the declared agent, so all expanded members of a
// pack-derived pool must report pack_derived=true with the same binding name.
func TestAgentListProvenancePoolInheritance(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{
			Name:              "polecat",
			Dir:               "myrig",
			BindingName:       "gastown",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 3",
		},
	}
	state.cfg.NamedSessions = nil
	state.rawCfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/tmp/myrig"},
		},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 3 {
		t.Fatalf("Total = %d, want 3 (bounded pool members)", resp.Total)
	}
	for i, item := range resp.Items {
		if !item.PackDerived {
			t.Errorf("Items[%d] (%s) PackDerived = false, want true", i, item.Name)
		}
		if item.Pack != "gastown" {
			t.Errorf("Items[%d] (%s) Pack = %q, want %q", i, item.Name, item.Pack, "gastown")
		}
	}
}

func TestLiveActiveBeadBatchesIdentitiesAndPreservesPrecedence(t *testing.T) {
	state := newFakeState(t)
	a := &activeBeadQueryStore{Store: &beads.MemStore{IDPrefix: "a"}}
	z := &activeBeadQueryStore{Store: &beads.MemStore{IDPrefix: "z"}}
	state.stores = map[string]beads.Store{"a": a, "z": z}
	srv := New(state)
	identities := []string{"session-id", "runtime-name", "alias", "template"}
	assertReads := func() {
		t.Helper()
		for name, store := range map[string]*activeBeadQueryStore{"a": a, "z": z} {
			if len(store.queries) != 1 {
				t.Errorf("store %s: %d live active-work reads, want one per store", name, len(store.queries))
			}
			for _, q := range store.queries {
				if !q.Live {
					t.Errorf("store %s used a stale lookup", name)
				}
			}
			store.queries = nil
		}
	}
	if got := srv.findLiveActiveBeadForAssignees("", identities...); got != "" {
		t.Fatalf("empty stores returned %q", got)
	}
	assertReads()
	alias := addActiveWork(t, a, "alias")
	addActiveWork(t, z, "session-id")
	newest := addActiveWork(t, z, "session-id")
	if got := srv.findLiveActiveBeadForAssignees("", identities...); got != newest {
		t.Fatalf("concrete session must win across rigs; got %q, want %q", got, newest)
	}
	assertReads()
	if got := srv.findLiveActiveBeadForAssignees("a", identities...); got != alias {
		t.Fatalf("rig-scoped alias lookup = %q, want %q", got, alias)
	}
	if len(a.queries) != 1 || len(z.queries) != 0 {
		t.Fatalf("rig filter queried a=%d z=%d times", len(a.queries), len(z.queries))
	}
}

func TestLiveActiveBeadStopsOnceTopIdentityMatches(t *testing.T) {
	state := newFakeState(t)
	a := &activeBeadQueryStore{Store: &beads.MemStore{IDPrefix: "a"}}
	z := &activeBeadQueryStore{Store: &beads.MemStore{IDPrefix: "z"}}
	state.stores = map[string]beads.Store{"a": a, "z": z}
	own := addActiveWork(t, a, "session-id")
	addActiveWork(t, z, "template")

	got := New(state).findLiveActiveBeadForAssignees("", "session-id", "template")
	if got != own {
		t.Fatalf("active bead = %q, want the session's own %q", got, own)
	}
	if len(a.queries) != 1 || len(z.queries) != 0 {
		t.Fatalf("reads a=%d z=%d, want 1 and 0: no later store can outrank the top identity", len(a.queries), len(z.queries))
	}
}

func TestLiveActiveBeadPrefersFirstStoreForSameIdentity(t *testing.T) {
	state := newFakeState(t)
	a := &beads.MemStore{IDPrefix: "a"}
	z := &beads.MemStore{IDPrefix: "z"}
	state.stores = map[string]beads.Store{"a": a, "z": z}
	first := addActiveWork(t, a, "alias")
	addActiveWork(t, z, "alias")

	if got := New(state).findLiveActiveBeadForAssignees("", "session-id", "alias"); got != first {
		t.Fatalf("active bead = %q, want %q from the first store, as on the cached path", got, first)
	}
}

func TestLiveActiveBeadIgnoresBlankIdentities(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.stores = map[string]beads.Store{"a": store}
	addActiveWork(t, store, "")

	if got := New(state).findLiveActiveBeadForAssignees("", "", "  "); got != "" {
		t.Fatalf("blank identities matched unassigned work %q", got)
	}
}

func TestLiveActiveBeadStoreErrors(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantOwn bool
	}{
		{
			name: "failed read skips the store",
			err:  errors.New("bd list: exit status 1"),
		},
		{
			name:    "partial result keeps decoded rows",
			err:     &beads.PartialResultError{Op: "bd list", Err: errors.New("decoding an unrelated bead")},
			wantOwn: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newFakeState(t)
			a := &beads.MemStore{IDPrefix: "a"}
			z := &beads.MemStore{IDPrefix: "z"}
			state.stores = map[string]beads.Store{"a": activeBeadErrStore{Store: a, err: tt.err}, "z": z}
			own := addActiveWork(t, a, "session-id")
			want := addActiveWork(t, z, "template")
			if tt.wantOwn {
				want = own
			}

			if got := New(state).findLiveActiveBeadForAssignees("", "session-id", "template"); got != want {
				t.Fatalf("active bead = %q, want %q", got, want)
			}
		})
	}
}

// listCountingStore counts every List call, so a test can prove a store was
// never read.
type listCountingStore struct {
	beads.Store
	listCalls int
}

func (s *listCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.List(query)
}

// suspendedRigLookup is a city with rigs alpha and beta, each holding
// in_progress work assigned to "session-id". alpha sorts first, so a lookup
// that reads it returns alpha's bead.
type suspendedRigLookup struct {
	state     *fakeState
	alpha     *listCountingStore
	beta      *listCountingStore
	alphaWork string
	betaWork  string
}

func newSuspendedRigLookup(t *testing.T) suspendedRigLookup {
	t.Helper()
	state := newFakeState(t)
	state.cfg.Rigs = []config.Rig{
		{Name: "alpha", Path: "/tmp/alpha"},
		{Name: "beta", Path: "/tmp/beta"},
	}
	alpha := &listCountingStore{Store: &beads.MemStore{IDPrefix: "alpha"}}
	beta := &listCountingStore{Store: &beads.MemStore{IDPrefix: "beta"}}
	state.stores = map[string]beads.Store{"alpha": alpha, "beta": beta}
	l := suspendedRigLookup{state: state, alpha: alpha, beta: beta}
	l.alphaWork = addActiveWork(t, alpha.Store, "session-id")
	l.betaWork = addActiveWork(t, beta.Store, "session-id")
	return l
}

// suspendAlphaAtRuntime suspends rig alpha in runtime state the way
// `gc rig suspend alpha` records it.
func suspendAlphaAtRuntime(t *testing.T, state *fakeState) {
	t.Helper()
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, state.cityPath, "alpha", &suspended); err != nil {
		t.Fatalf("SetRigSuspended(alpha): %v", err)
	}
}

// lookupActiveBead runs the cached or live active-bead lookup.
func lookupActiveBead(srv *Server, live bool, rig string, assignees ...string) string {
	if live {
		return srv.findLiveActiveBeadForAssignees(rig, assignees...)
	}
	return srv.findActiveBeadForAssignees(rig, assignees...)
}

// TestActiveBeadLookupSkipsSuspendedRig: a rig suspended by effective state
// has no cache, so any read of its store goes to bd and restarts its retired
// proxy and Dolt (#8162). The unscoped lookup searches the other rigs only.
func TestActiveBeadLookupSkipsSuspendedRig(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
	}{{"live", true}, {"cached", false}} {
		t.Run(tc.name, func(t *testing.T) {
			l := newSuspendedRigLookup(t)
			suspendAlphaAtRuntime(t, l.state)

			got := lookupActiveBead(New(l.state), tc.live, "", "session-id")

			if l.alpha.listCalls != 0 {
				t.Errorf("suspended rig alpha List calls = %d, want 0", l.alpha.listCalls)
			}
			if got != l.betaWork {
				t.Errorf("active bead = %q, want beta's %q", got, l.betaWork)
			}
		})
	}
}

// TestActiveBeadLookupForSuspendedRigReadsNoStore: a rig-scoped lookup for a
// suspended rig whose store no unsuspended rig shares answers nothing instead
// of falling back to every rig. That holds for a store of its own and for one
// it shares only with another suspended rig.
func TestActiveBeadLookupForSuspendedRigReadsNoStore(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		shareWithSuspendedRig bool
	}{{"own store", false}, {"store shared with a suspended rig", true}} {
		for _, live := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/live=%v", tc.name, live), func(t *testing.T) {
				l := newSuspendedRigLookup(t)
				suspendAlphaAtRuntime(t, l.state)
				if tc.shareWithSuspendedRig {
					l.state.cfg.Rigs = append(l.state.cfg.Rigs, config.Rig{Name: "gamma", Path: "/tmp/gamma", SuspendedOnStart: true})
					l.state.stores["gamma"] = l.alpha
				}

				got := lookupActiveBead(New(l.state), live, "alpha", "session-id")

				if got != "" {
					t.Errorf("active bead = %q, want none", got)
				}
				if l.alpha.listCalls != 0 || l.beta.listCalls != 0 {
					t.Errorf("List calls alpha=%d beta=%d, want no store read", l.alpha.listCalls, l.beta.listCalls)
				}
			})
		}
	}
}

// TestActiveBeadLookupUsesEffectiveRigSuspension: the skipped set is the one
// statusWorkCounts skips. A runtime override wins; without one, the config's
// suspended_on_start (or legacy suspended) decides.
func TestActiveBeadLookupUsesEffectiveRigSuspension(t *testing.T) {
	tests := []struct {
		name      string
		rig       config.Rig
		runtime   *bool
		wantAlpha bool
	}{
		{
			name: "suspended_on_start without runtime override",
			rig:  config.Rig{Name: "alpha", Path: "/tmp/alpha", SuspendedOnStart: true},
		},
		{
			name:      "suspended_on_start with runtime resume",
			rig:       config.Rig{Name: "alpha", Path: "/tmp/alpha", SuspendedOnStart: true},
			runtime:   boolPtr(false),
			wantAlpha: true,
		},
		{
			name: "legacy suspended",
			rig:  config.Rig{Name: "alpha", Path: "/tmp/alpha", Suspended: true},
		},
		{
			name:    "runtime suspend over suspended_on_start false",
			rig:     config.Rig{Name: "alpha", Path: "/tmp/alpha"},
			runtime: boolPtr(true),
		},
	}
	for _, tt := range tests {
		for _, live := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/live=%v", tt.name, live), func(t *testing.T) {
				l := newSuspendedRigLookup(t)
				l.state.cfg.Rigs[0] = tt.rig
				if tt.runtime != nil {
					if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, l.state.cityPath, "alpha", tt.runtime); err != nil {
						t.Fatalf("SetRigSuspended(alpha, %v): %v", *tt.runtime, err)
					}
				}

				got := lookupActiveBead(New(l.state), live, "", "session-id")

				want := l.betaWork
				if tt.wantAlpha {
					want = l.alphaWork
				}
				if read := l.alpha.listCalls > 0; read != tt.wantAlpha {
					t.Errorf("alpha read = %v (List calls %d), want %v", read, l.alpha.listCalls, tt.wantAlpha)
				}
				if got != want {
					t.Errorf("active bead = %q, want %q", got, want)
				}
			})
		}
	}
}

// TestActiveBeadLookupReadsRigInferredSuspended: a rig that status infers
// suspended because all of its agents are keeps a refreshing cache and is
// still read.
func TestActiveBeadLookupReadsRigInferredSuspended(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			l := newSuspendedRigLookup(t)
			l.state.cfg.Rigs = l.state.cfg.Rigs[1:]
			delete(l.state.stores, "alpha")
			l.state.cfg.Agents = []config.Agent{
				{Name: "worker", Dir: "beta", Provider: "test-agent", Suspended: true},
			}
			l.state.cfg.NamedSessions = nil

			got := lookupActiveBead(New(l.state), live, "", "session-id")

			if l.beta.listCalls == 0 {
				t.Error("beta was not read")
			}
			if got != l.betaWork {
				t.Errorf("active bead = %q, want beta's %q", got, l.betaWork)
			}
		})
	}
}

// TestActiveBeadLookupReadsSharedStoreThroughUnsuspendedAlias: in legacy
// shared-file mode every rig aliases one store, which the controller keeps
// warm even for a suspended rig. A suspended alpha must not hide the store
// that beta still reaches, whether the lookup is unscoped or scoped to alpha.
func TestActiveBeadLookupReadsSharedStoreThroughUnsuspendedAlias(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("unscoped/live=%v", live), func(t *testing.T) {
			l := newSuspendedRigLookup(t)
			suspendAlphaAtRuntime(t, l.state)
			l.state.stores["alpha"] = l.beta

			got := lookupActiveBead(New(l.state), live, "", "session-id")

			if l.beta.listCalls == 0 {
				t.Error("shared store was not read through beta")
			}
			if got != l.betaWork {
				t.Errorf("active bead = %q, want %q", got, l.betaWork)
			}
		})
		// The scoped lookup reads the shared store and nothing else: able
		// has a store of its own and sorts first, so a lookup that fell
		// back to every store would answer able's bead.
		t.Run(fmt.Sprintf("scoped to alpha/live=%v", live), func(t *testing.T) {
			l := newSuspendedRigLookup(t)
			suspendAlphaAtRuntime(t, l.state)
			l.state.stores["alpha"] = l.beta
			able := &listCountingStore{Store: &beads.MemStore{IDPrefix: "able"}}
			addActiveWork(t, able.Store, "session-id")
			l.state.cfg.Rigs = append(l.state.cfg.Rigs, config.Rig{Name: "able", Path: "/tmp/able"})
			l.state.stores["able"] = able

			got := lookupActiveBead(New(l.state), live, "alpha", "session-id")

			if able.listCalls != 0 {
				t.Errorf("rig able List calls = %d, want 0", able.listCalls)
			}
			if got != l.betaWork {
				t.Errorf("active bead = %q, want the shared store's %q", got, l.betaWork)
			}
		})
	}
}

// TestActiveBeadLookupSuspensionStateEdges: an unreadable state file or an
// unknown city path still applies the config defaults, neither skipping nor
// reading every rig wholesale.
func TestActiveBeadLookupSuspensionStateEdges(t *testing.T) {
	t.Run("corrupt state file", func(t *testing.T) {
		l := newSuspendedRigLookup(t)
		l.state.cfg.Rigs[0].SuspendedOnStart = true
		path := citylayout.SuspensionStateFile(l.state.cityPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}

		got := New(l.state).findActiveBeadForAssignees("", "session-id")

		if l.alpha.listCalls != 0 {
			t.Errorf("suspended_on_start rig alpha List calls = %d, want 0", l.alpha.listCalls)
		}
		if l.beta.listCalls == 0 || got != l.betaWork {
			t.Errorf("active bead = %q (beta List calls %d), want beta's %q", got, l.beta.listCalls, l.betaWork)
		}
	})
	t.Run("empty city path", func(t *testing.T) {
		l := newSuspendedRigLookup(t)
		l.state.cfg.Rigs[0].SuspendedOnStart = true
		l.state.cityPath = ""
		cwd := t.TempDir()
		t.Chdir(cwd)
		relative := citylayout.SuspensionStateFile("")
		if err := os.MkdirAll(filepath.Dir(relative), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(relative, []byte(`{"rigs":{"beta":{"suspended":true}}}`), 0o644); err != nil {
			t.Fatal(err)
		}

		got := New(l.state).findLiveActiveBeadForAssignees("", "session-id")

		if l.alpha.listCalls != 0 {
			t.Errorf("suspended_on_start rig alpha List calls = %d, want 0", l.alpha.listCalls)
		}
		if got != l.betaWork {
			t.Errorf("active bead = %q, want beta's %q (a relative state file must not be read)", got, l.betaWork)
		}
	})
}

// TestActiveBeadLookupLogsUnreadableSuspensionStateOnce: every lookup reads
// the state file, once per row of a list, so an unreadable file is logged
// once, naming the file, until it reads again. A fault after that is logged
// again.
func TestActiveBeadLookupLogsUnreadableSuspensionStateOnce(t *testing.T) {
	l := newSuspendedRigLookup(t)
	path := citylayout.SuspensionStateFile(l.state.cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeState := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(l.state)
	lookupRows := func() {
		for range 3 {
			srv.findActiveBeadForAssignees("", "session-id")
		}
	}
	var logs bytes.Buffer
	t.Cleanup(captureLog(t, &logs))

	writeState("{not json")
	lookupRows()
	if n := strings.Count(logs.String(), path); n != 1 {
		t.Fatalf("corrupt state file logged %d times naming %s, want 1; log:\n%s", n, path, logs.String())
	}

	writeState(`{}`)
	lookupRows()
	writeState("{not json")
	lookupRows()
	if n := strings.Count(logs.String(), path); n != 2 {
		t.Fatalf("state file corrupt again after it read logged %d times in all, want 2; log:\n%s", n, logs.String())
	}
}
