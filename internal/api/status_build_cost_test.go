package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// largeCityShape is the agent-config shape of a large city (maintainer-city,
// ga-v1vyh8.20): hundreds of unlimited-pool templates imported per rig, of
// which only a handful hold a live session.
type largeCityShape struct {
	rigs              int
	unlimitedPerRig   int
	unlimitedCity     int
	boundedPerRig     int
	boundedCity       int
	singletonsCity    int
	singletonsPerRig  int
	livePools         int // unlimited pools with live instances
	instancesPerLive  int
	runningSingletons int
	listLatency       time.Duration // per ListRunning call (one tmux fork)
	probeLatency      time.Duration // per show-environment / list-windows
	storeLatency      time.Duration // per bead-store List
}

// maintainerCityShape mirrors maintainer-city: 7 rigs, 676 unlimited pools,
// 45 bounded pools, 60 singletons; 9 pools hold live instances.
var maintainerCityShape = largeCityShape{
	rigs:              7,
	unlimitedPerRig:   96,
	unlimitedCity:     4,
	boundedPerRig:     6,
	boundedCity:       3,
	singletonsCity:    40,
	singletonsPerRig:  3,
	livePools:         9,
	instancesPerLive:  2,
	runningSingletons: 20,
}

func (s largeCityShape) unlimitedPools() int {
	return s.rigs*s.unlimitedPerRig + s.unlimitedCity
}

// costProvider is the session provider of the large city: a runtime.Fake that
// lists in name order (as tmux does), counts each ListRunning by prefix, and
// pays latency per call — the cost of one `tmux list-sessions` fork.
type costProvider struct {
	*runtime.Fake
	latency time.Duration
	// probeLatency is the cost of one per-session tmux read: a
	// show-environment (GetAllEnvironment) or a list-windows (each roster
	// session's activity).
	probeLatency time.Duration
	envReads     atomic.Int64
	rosterReads  atomic.Int64
	// failFullListing makes ListRunning("") fail while prefix listings
	// succeed, forcing per-prefix answers.
	failFullListing bool

	mu       sync.Mutex
	prefixes []string
}

// payLatency models the wall-clock cost of one runtime fork or store read
// in the large-city cost model; zero costs nothing.
func payLatency(d time.Duration) {
	if d > 0 {
		time.Sleep(d)
	}
}

func (p *costProvider) ListRunning(prefix string) ([]string, error) {
	p.mu.Lock()
	p.prefixes = append(p.prefixes, prefix)
	p.mu.Unlock()
	payLatency(p.latency)
	if p.failFullListing && prefix == "" {
		return nil, errors.New("full listing refused")
	}
	names, err := p.Fake.ListRunning(prefix)
	slices.Sort(names)
	return names, err
}

// GetAllEnvironment models tmux's one show-environment fork per session.
func (p *costProvider) GetAllEnvironment(name string) (map[string]string, error) {
	p.envReads.Add(1)
	payLatency(p.probeLatency)
	suspended, _ := p.GetMeta(name, "suspended")
	id, _ := p.GetMeta(name, "GC_SESSION_ID")
	return map[string]string{"suspended": suspended, "GC_SESSION_ID": id}, nil
}

// SessionRoster models tmux's roster: one list-sessions, then one
// list-windows per session for its activity.
func (p *costProvider) SessionRoster() (map[string]runtime.SessionRosterEntry, error) {
	names, err := p.Fake.ListRunning("")
	if err != nil {
		return nil, err
	}
	roster := make(map[string]runtime.SessionRosterEntry, len(names))
	for _, name := range names {
		p.rosterReads.Add(1)
		payLatency(p.probeLatency)
		roster[name] = runtime.SessionRosterEntry{}
	}
	return roster, nil
}

func (p *costProvider) listCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.prefixes)
}

func (p *costProvider) reset() {
	p.mu.Lock()
	p.prefixes = nil
	p.mu.Unlock()
}

// costStore is a rig's bead store as the API holds it: List pays latency (a
// cache scan, or a remote read when the cache cannot serve) and is counted,
// and, like the bead-policy wrapper over the production cache, it offers no
// CachedList, so every active-bead lookup lists.
type costStore struct {
	beads.Store
	latency time.Duration
	lists   *atomic.Int64
}

func (s costStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	payLatency(s.latency)
	return s.Store.List(query)
}

// newLargeCityState builds a fakeState with shape's config and live sessions.
func newLargeCityState(t testing.TB, shape largeCityShape) (*fakeState, *costProvider) {
	state, sp, _ := newLargeCityStateWithStores(t, shape)
	return state, sp
}

func newLargeCityStateWithStores(t testing.TB, shape largeCityShape) (*fakeState, *costProvider, *atomic.Int64) {
	t.Helper()
	state := newFakeState(t)
	cfg := &config.City{Workspace: config.Workspace{Name: state.cityName}}
	stores := map[string]beads.Store{}
	storeLists := &atomic.Int64{}
	for r := 0; r < shape.rigs; r++ {
		name := fmt.Sprintf("rig%d", r)
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: "/tmp/" + name})
		mem := beads.NewMemStore()
		for i := 0; i < 200; i++ {
			b, err := mem.Create(beads.Bead{Type: "task", Title: fmt.Sprintf("work %d", i)})
			if err != nil {
				t.Fatal(err)
			}
			if i%10 == 0 {
				status, assignee := "in_progress", fmt.Sprintf("%s/u%03d-1", name, i/10)
				if err := mem.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
					t.Fatal(err)
				}
			}
		}
		stores[name] = costStore{Store: mem, latency: shape.storeLatency, lists: storeLists}
	}
	addAgents := func(dir string, unlimited, bounded, singletons int) {
		for i := 0; i < unlimited; i++ {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("u%03d", i), Dir: dir, Provider: "test-agent"})
		}
		for i := 0; i < bounded; i++ {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("b%02d", i), Dir: dir, Provider: "test-agent", MaxActiveSessions: intPtr(2 + i%4)})
		}
		for i := 0; i < singletons; i++ {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("s%02d", i), Dir: dir, Provider: "test-agent", MaxActiveSessions: intPtr(1)})
		}
	}
	addAgents("", shape.unlimitedCity, shape.boundedCity, shape.singletonsCity)
	for r := 0; r < shape.rigs; r++ {
		addAgents(fmt.Sprintf("rig%d", r), shape.unlimitedPerRig, shape.boundedPerRig, shape.singletonsPerRig)
	}
	cfg.Providers = map[string]config.ProviderSpec{"test-agent": {DisplayName: "Test Agent"}}
	state.cfg = cfg
	state.stores = stores

	fake := runtime.NewFake()
	start := func(qualifiedName string) {
		if err := fake.Start(context.Background(), agent.SessionNameFor(state.cityName, qualifiedName, ""), runtime.Config{}); err != nil {
			t.Fatalf("start %s: %v", qualifiedName, err)
		}
	}
	for i := 0; i < shape.livePools; i++ {
		rig := fmt.Sprintf("rig%d", i%shape.rigs)
		for n := 1; n <= shape.instancesPerLive; n++ {
			start(fmt.Sprintf("%s/u%03d-%d", rig, i, n))
		}
	}
	for i := 0; i < shape.runningSingletons && i < shape.singletonsCity; i++ {
		start(fmt.Sprintf("s%02d", i))
	}
	sp := &costProvider{Fake: fake, latency: shape.listLatency, probeLatency: shape.probeLatency}
	state.sessionProvider = sp
	return state, sp, storeLists
}

func newLargeCityServer(state *fakeState) *Server {
	s := New(state)
	s.componentVersionsProbe = func() componentVersions { return componentVersions{} }
	return s
}

// A status build lists the runtime's sessions once, whatever the number of
// unlimited pools it expands (ga-v1vyh8.20). Before the fix every unlimited
// pool forked its own `tmux list-sessions`.
func TestStatusBuildListsRuntimeSessionsOncePerBuild(t *testing.T) {
	for _, lite := range []bool{true, false} {
		t.Run(fmt.Sprintf("lite=%v", lite), func(t *testing.T) {
			state, sp := newLargeCityState(t, maintainerCityShape)
			s := newLargeCityServer(state)
			body := s.buildStatusBody(context.Background(), lite)
			if got := sp.listCalls(); got != 1 {
				t.Fatalf("ListRunning calls per build = %d, want 1 (unlimited pools: %d)", got, maintainerCityShape.unlimitedPools())
			}
			wantLive := maintainerCityShape.livePools * maintainerCityShape.instancesPerLive
			gotLive := 0
			for _, d := range body.AgentDetails {
				if d.Expanded && d.Running {
					gotLive++
				}
			}
			if gotLive != wantLive {
				t.Fatalf("running expanded rows = %d, want %d live pool instances", gotLive, wantLive)
			}
		})
	}
}

// A city with no unlimited pool lists nothing: the listing is read on the
// first pool that needs it (TestHandleStatusOnlyUsesProviderLiveness).
func TestStatusBuildWithoutUnlimitedPoolsListsNothing(t *testing.T) {
	shape := maintainerCityShape
	shape.unlimitedPerRig, shape.unlimitedCity, shape.livePools = 0, 0, 0
	state, sp := newLargeCityState(t, shape)
	newLargeCityServer(state).buildStatusBody(context.Background(), true)
	if got := sp.listCalls(); got != 0 {
		t.Fatalf("ListRunning calls = %d, want 0 without unlimited pools", got)
	}
}

func getJSON(t *testing.T, h http.Handler, url string, out any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// GET /agents and GET /rigs list the runtime's sessions once per request.
func TestAgentAndRigListsListRuntimeSessionsOncePerRequest(t *testing.T) {
	state, sp := newLargeCityState(t, maintainerCityShape)
	h := newTestCityHandlerWith(t, state, newLargeCityServer(state))

	var agents agentListWire
	getJSON(t, h, cityURL(state, "/agents"), &agents)
	if got := sp.listCalls(); got != 1 {
		t.Errorf("/agents ListRunning calls = %d, want 1", got)
	}
	if agents.Partial {
		t.Errorf("/agents partial: %v", agents.PartialErrors)
	}

	sp.reset()
	var rigs struct {
		Items []rigResponse `json:"items"`
	}
	getJSON(t, h, cityURL(state, "/rigs"), &rigs)
	if got := sp.listCalls(); got != 1 {
		t.Errorf("/rigs ListRunning calls = %d, want 1", got)
	}
}

// statusBodyJSON marshals body without its clock-derived field.
func statusBodyJSON(t *testing.T, body StatusBody) string {
	t.Helper()
	body.UptimeSec = 0
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Answering every pool from one listing is the per-prefix answer: the body
// built when each pool asks the provider itself (a failed full listing) is
// byte-identical to the body built from the shared listing, nested pool
// prefixes included.
func TestStatusBuildSharedListingMatchesPerPrefixListing(t *testing.T) {
	state, sp := newLargeCityState(t, maintainerCityShape)
	// A pool whose prefix nests in another's: rig0/u000-x's sessions also
	// carry rig0/u000's prefix.
	state.cfg.Agents = append(state.cfg.Agents, config.Agent{Name: "u000-x", Dir: "rig0", Provider: "test-agent"})
	if err := sp.Start(context.Background(), agent.SessionNameFor(state.cityName, "rig0/u000-x-1", ""), runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	build := func(failFull bool) (StatusBody, string) {
		sp.failFullListing = failFull
		body := newLargeCityServer(state).buildStatusBody(context.Background(), false)
		return body, statusBodyJSON(t, body)
	}
	shared, sharedJSON := build(false)
	_, perPrefixJSON := build(true)
	if sharedJSON != perPrefixJSON {
		t.Fatalf("status body differs between shared and per-prefix listing:\nshared:     %s\nper-prefix: %s", sharedJSON, perPrefixJSON)
	}
	if len(shared.AgentDetails) == 0 {
		t.Fatal("no agent rows")
	}
}

// The same holds for GET /agents: the list body built from the shared listing
// is byte-identical to the one built from per-prefix listings.
func TestAgentListSharedListingMatchesPerPrefixListing(t *testing.T) {
	state, sp := newLargeCityState(t, maintainerCityShape)
	state.cfg.Agents = append(state.cfg.Agents, config.Agent{Name: "u000-x", Dir: "rig0", Provider: "test-agent"})
	if err := sp.Start(context.Background(), agent.SessionNameFor(state.cityName, "rig0/u000-x-1", ""), runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	build := func(failFull bool) string {
		sp.failFullListing = failFull
		out, err := newLargeCityServer(state).humaHandleAgentList(context.Background(), &AgentListInput{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Body.Partial {
			t.Fatalf("partial agent list (failFull=%v): %q", failFull, out.Body.PartialErrors)
		}
		raw, err := json.Marshal(out.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	shared, perPrefix := build(false), build(true)
	if shared != perPrefix {
		t.Fatalf("agent list differs between shared and per-prefix listing:\nshared:     %s\nper-prefix: %s", shared, perPrefix)
	}
}

// prefixScopedErrorProvider fails the full listing with a partial error, as a
// composite does when one backend session cannot be classified, while a pool
// whose prefix does not cover that session lists cleanly.
type prefixScopedErrorProvider struct {
	*runtime.Fake
	brokenPrefix string
}

func (p *prefixScopedErrorProvider) ListRunning(prefix string) ([]string, error) {
	names, _ := p.Fake.ListRunning(prefix)
	slices.Sort(names)
	if prefix == "" || prefix == p.brokenPrefix {
		return names, &runtime.PartialListError{Err: errors.New("acp backend: socket unclassifiable")}
	}
	return names, nil
}

// A failed full listing leaves each pool its own answer, as before: the
// healthy pool keeps its instances and only the broken pool reports.
func TestAgentListPartialFullListingKeepsPerPoolAnswers(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{Name: "good", Dir: "myrig", Provider: "test-agent"},
		{Name: "bad", Dir: "myrig", Provider: "test-agent"},
	}
	fake := runtime.NewFake()
	for _, qn := range []string{"myrig/good-1", "myrig/bad-1"} {
		if err := fake.Start(context.Background(), agent.SessionNameFor(state.cityName, qn, ""), runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	state.sessionProvider = &prefixScopedErrorProvider{Fake: fake, brokenPrefix: agent.SessionNameFor(state.cityName, "myrig/bad-", "")}
	resp := getAgentList(t, state, "")
	if _, ok := agentByNameIn(resp.Items, "myrig/good-1"); !ok {
		t.Fatalf("myrig/good-1 missing; items = %+v", resp.Items)
	}
	if _, ok := agentByNameIn(resp.Items, "myrig/bad-1"); ok {
		t.Fatalf("myrig/bad-1 listed despite its pool's failed listing")
	}
	if len(resp.PartialErrors) != 1 {
		t.Fatalf("PartialErrors = %q, want one for the broken pool", resp.PartialErrors)
	}
}

// BenchmarkStatusBuildLargeCity measures a maintainer-city-shaped status
// build with a 1ms ListRunning (a tmux fork, scaled down).
func BenchmarkStatusBuildLargeCity(b *testing.B) {
	for _, lite := range []bool{true, false} {
		b.Run(fmt.Sprintf("lite=%v", lite), func(b *testing.B) {
			shape := maintainerCityShape
			shape.listLatency = time.Millisecond
			state, sp := newLargeCityState(b, shape)
			s := newLargeCityServer(state)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.buildStatusBody(context.Background(), lite)
			}
			b.ReportMetric(float64(sp.listCalls())/float64(b.N), "listruns/op")
		})
	}
}

// BenchmarkAgentListLargeCity measures a maintainer-city-shaped GET /agents
// build: 1ms per tmux fork (list-sessions, show-environment, list-windows)
// and 100us per bead-store List.
func BenchmarkAgentListLargeCity(b *testing.B) {
	shape := maintainerCityShape
	shape.listLatency, shape.probeLatency, shape.storeLatency = time.Millisecond, time.Millisecond, 100*time.Microsecond
	state, sp, storeLists := newLargeCityStateWithStores(b, shape)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := New(state).humaHandleAgentList(context.Background(), &AgentListInput{}); err != nil {
			b.Fatal(err)
		}
	}
	n := float64(b.N)
	b.ReportMetric(float64(sp.listCalls())/n, "listruns/op")
	b.ReportMetric(float64(sp.envReads.Load())/n, "envreads/op")
	b.ReportMetric(float64(sp.rosterReads.Load())/n, "rosterwindows/op")
	b.ReportMetric(float64(storeLists.Load())/n, "storelists/op")
}
