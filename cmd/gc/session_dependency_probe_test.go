package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// depProbeSeq makes every fixture's session names unique: worker.ObserveBounded
// keeps its in-flight map at package level, so two tests that shared a city
// path and a session name could see each other's outstanding observation.
var depProbeSeq atomic.Int64

// depProbeProvider is a runtime whose liveness observation of a chosen session
// blocks until the test releases it, the way a wedged tmux subprocess does. It
// counts the calls it receives per session.
type depProbeProvider struct {
	*runtime.Fake

	mu       sync.Mutex
	wedge    map[string]chan struct{}
	observed map[string]int
	calls    map[string]int
}

func newDepProbeProvider(t *testing.T) *depProbeProvider {
	t.Helper()
	p := &depProbeProvider{
		Fake:     runtime.NewFake(),
		wedge:    map[string]chan struct{}{},
		observed: map[string]int{},
		calls:    map[string]int{},
	}
	t.Cleanup(p.releaseAll)
	return p
}

// wedgeSession makes every liveness observation of name block until releaseAll.
func (p *depProbeProvider) wedgeSession(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wedge[name] = make(chan struct{})
}

// releaseAll lets every blocked observation return. It is idempotent.
func (p *depProbeProvider) releaseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, gate := range p.wedge {
		close(gate)
		delete(p.wedge, name)
	}
}

func (p *depProbeProvider) note(name string, observation bool) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[name]++
	if !observation {
		return nil
	}
	p.observed[name]++
	return p.wedge[name]
}

// ObserveLivenessWithError is the observation every liveness check goes
// through; it blocks while name is wedged and then answers from the fake.
func (p *depProbeProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) { //nolint:unparam // runtime.LivenessObserverWithError fixes the signature; the fake observation never fails
	if gate := p.note(name, true); gate != nil {
		<-gate
	}
	running := p.Fake.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

func (p *depProbeProvider) IsRunning(name string) bool {
	p.note(name, false)
	return p.Fake.IsRunning(name)
}

func (p *depProbeProvider) ProcessAlive(name string, processNames []string) bool {
	p.note(name, false)
	return p.Fake.ProcessAlive(name, processNames)
}

func (p *depProbeProvider) GetMeta(name, key string) (string, error) {
	p.note(name, false)
	return p.Fake.GetMeta(name, key)
}

func (p *depProbeProvider) observationCount(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.observed[name]
}

func (p *depProbeProvider) callCount(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[name]
}

// depCountingStore records every List query and every Get id that reaches the
// wrapped store.
type depCountingStore struct {
	beads.Store

	mu    sync.Mutex
	lists []string
	gets  []string
}

func (s *depCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.lists = append(s.lists, fmt.Sprintf("%#v", q))
	s.mu.Unlock()
	return s.Store.List(q)
}

func (s *depCountingStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	s.gets = append(s.gets, id)
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *depCountingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists = nil
	s.gets = nil
}

func (s *depCountingStore) listsMatching(shapes map[string]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.lists {
		if shapes[q] {
			n++
		}
	}
	return n
}

func (s *depCountingStore) getCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, got := range s.gets {
		if got == id {
			n++
		}
	}
	return n
}

// openSessionListShapes returns the List queries one loadOpenSessionInfos
// issues on store, so a test can count open-session lists without naming the
// union's legs.
func openSessionListShapes(t *testing.T, store *depCountingStore) map[string]bool {
	t.Helper()
	store.reset()
	if _, err := loadOpenSessionInfos(store); err != nil {
		t.Fatalf("calibrating loadOpenSessionInfos: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	shapes := map[string]bool{}
	for _, q := range store.lists {
		shapes[q] = true
	}
	if len(shapes) == 0 {
		t.Fatal("loadOpenSessionInfos issued no List call to calibrate against")
	}
	return shapes
}

// depProbeEnv is a city whose dependents all depend on every dependency, each a
// single-session template whose session name is its template name.
type depProbeEnv struct {
	t          *testing.T
	cityPath   string
	cfg        *config.City
	desired    map[string]TemplateParams
	store      *depCountingStore
	sp         *depProbeProvider
	dependents []string
	deps       []string
	depBead    map[string]string
}

// newDepProbeEnv builds nDependents dependents sharing nDeps dependencies.
// Every dependency has one open session bead and a running runtime.
func newDepProbeEnv(t *testing.T, nDependents, nDeps int) *depProbeEnv {
	t.Helper()
	seq := depProbeSeq.Add(1)
	env := &depProbeEnv{
		t:        t,
		cityPath: t.TempDir(),
		desired:  map[string]TemplateParams{},
		store:    &depCountingStore{Store: beads.NewMemStore()},
		sp:       newDepProbeProvider(t),
		depBead:  map[string]string{},
	}
	for j := range nDeps {
		env.deps = append(env.deps, fmt.Sprintf("dpdb%d-%d", j, seq))
	}
	for i := range nDependents {
		env.dependents = append(env.dependents, fmt.Sprintf("dpapp%d-%d", i, seq))
	}
	env.cfg = &config.City{Workspace: config.Workspace{SessionTemplate: "{{.Agent}}"}}
	for _, dep := range env.deps {
		env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: dep, MaxActiveSessions: intPtr(1)})
		env.desired[dep] = TemplateParams{TemplateName: dep, SessionName: dep}
		env.addBead(dep)
		env.startRuntime(dep)
	}
	for _, app := range env.dependents {
		env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: app, MaxActiveSessions: intPtr(1), DependsOn: env.deps})
	}
	return env
}

// addBead gives the session name an open session bead and returns its id.
func (e *depProbeEnv) addBead(name string) string {
	e.t.Helper()
	b, err := e.store.Create(beads.Bead{
		Title:  name,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": name,
			"template":     name,
			"state":        "active",
		},
	})
	if err != nil {
		e.t.Fatalf("create session bead for %q: %v", name, err)
	}
	if _, ok := e.depBead[name]; !ok {
		e.depBead[name] = b.ID
	}
	return b.ID
}

func (e *depProbeEnv) startRuntime(name string) {
	e.t.Helper()
	if err := e.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		e.t.Fatalf("start runtime %q: %v", name, err)
	}
}

func (e *depProbeEnv) candidates(apps ...string) []startCandidate {
	out := make([]startCandidate, 0, len(apps))
	for i, app := range apps {
		out = append(out, startCandidate{
			info:  sessionpkg.Info{SessionName: app, SessionNameMetadata: app, Template: app},
			tp:    TemplateParams{TemplateName: app},
			order: i,
		})
	}
	return out
}

func (e *depProbeEnv) pass(ctx context.Context, log *bytes.Buffer) *dependencyProbePass {
	if log == nil {
		log = &bytes.Buffer{}
	}
	return newDependencyProbePass(ctx, e.cityPath, log)
}

// allAlive is the wave first filter's and the recheck's question for one
// dependent, asked in pass.
func (e *depProbeEnv) allAlive(app string, pass *dependencyProbePass) bool {
	return allDependenciesAliveForTemplateWithClock(app, e.cfg, e.desired, e.sp, "city", e.store, clock.Real{}, pass)
}

// waveOrder is the planner's candidateWaveOrder stage, run in pass.
func (e *depProbeEnv) waveOrder(pass *dependencyProbePass, apps ...string) {
	e.t.Helper()
	if _, ok := candidateWaveOrder(e.candidates(apps...), e.cfg, e.desired, e.sp, "city", e.store, clock.Real{}, pass); !ok {
		e.t.Fatal("candidateWaveOrder fell back to serial order")
	}
}

// shrinkDependencyProbeBound sets dependencyProbeBound to d for the test. The
// variable is package level, so the test must not run in parallel.
func shrinkDependencyProbeBound(t *testing.T, d time.Duration) {
	t.Helper()
	old := dependencyProbeBound
	dependencyProbeBound = d
	t.Cleanup(func() { dependencyProbeBound = old })
}

// runWithin runs fn on its own goroutine and reports whether it returned within
// limit and how long it took. A call that has not returned is left running; the
// provider double releases it in t.Cleanup. Where main would block for ever, a
// test that used it fails after limit instead of hanging.
func runWithin[T any](limit time.Duration, fn func() T) (got T, returned bool, took time.Duration) {
	done := make(chan T, 1)
	start := time.Now()
	go func() { done <- fn() }()
	select {
	case got = <-done:
		return got, true, time.Since(start)
	case <-time.After(limit):
		return got, false, time.Since(start)
	}
}

// TestDependencyProbe_OneOpenSessionListPerPass pins that a pass lists the open
// sessions once however many dependency checks it makes: K dependents sharing M
// dependency sessions made one list per check on main.
func TestDependencyProbe_OneOpenSessionListPerPass(t *testing.T) {
	const dependents, deps = 4, 3
	env := newDepProbeEnv(t, dependents, deps)
	shapes := openSessionListShapes(t, env.store)
	listsPerRead := env.store.listsMatching(shapes)

	t.Run("candidateWaveOrder", func(t *testing.T) {
		env.store.reset()
		env.waveOrder(env.pass(context.Background(), nil), env.dependents...)
		if got := env.store.listsMatching(shapes); got != listsPerRead {
			t.Fatalf("candidateWaveOrder pass made %d open-session list queries, want %d (one list)", got, listsPerRead)
		}
	})
	t.Run("first filter", func(t *testing.T) {
		env.store.reset()
		pass := env.pass(context.Background(), nil)
		for _, app := range env.dependents {
			if !env.allAlive(app, pass) {
				t.Fatalf("dependent %s should see its live dependencies", app)
			}
		}
		if got := env.store.listsMatching(shapes); got != listsPerRead {
			t.Fatalf("first filter over %d dependents made %d open-session list queries, want %d (one list)", dependents, got, listsPerRead)
		}
	})
	t.Run("recheck batch", func(t *testing.T) {
		env.store.reset()
		pass := env.pass(context.Background(), nil)
		for _, app := range env.dependents[:defaultStartDependencyRecheckBatchSize] {
			if !env.allAlive(app, pass) {
				t.Fatalf("dependent %s should see its live dependencies", app)
			}
		}
		if got := env.store.listsMatching(shapes); got != listsPerRead {
			t.Fatalf("recheck batch made %d open-session list queries, want %d (one list)", got, listsPerRead)
		}
	})
	t.Run("a new pass lists again", func(t *testing.T) {
		env.store.reset()
		env.allAlive(env.dependents[0], env.pass(context.Background(), nil))
		env.allAlive(env.dependents[0], env.pass(context.Background(), nil))
		if got := env.store.listsMatching(shapes); got != 2*listsPerRead {
			t.Fatalf("two passes made %d open-session list queries, want %d: a pass must not reuse an earlier pass's list", got, 2*listsPerRead)
		}
	})
}

// TestDependencyProbe_ProbesEachDependencyOncePerPass pins that a dependency
// session shared by N dependents costs the provider and the store what it costs
// with one dependent: main probed it once per dependent.
func TestDependencyProbe_ProbesEachDependencyOncePerPass(t *testing.T) {
	one := newDepProbeEnv(t, 1, 3)
	one.waveOrder(one.pass(context.Background(), nil), one.dependents...)

	many := newDepProbeEnv(t, 5, 3)
	many.waveOrder(many.pass(context.Background(), nil), many.dependents...)

	for j := range one.deps {
		oneName, manyName := one.deps[j], many.deps[j]
		if got := many.sp.observationCount(manyName); got != 1 || one.sp.observationCount(oneName) != 1 {
			t.Errorf("dependency %d observed %d times for 5 dependents (%d for 1), want exactly once per pass", j, got, one.sp.observationCount(oneName))
		}
		if got, want := many.sp.callCount(manyName), one.sp.callCount(oneName); got != want {
			t.Errorf("dependency %d took %d provider calls for 5 dependents, want the %d one dependent takes", j, got, want)
		}
		if got, want := many.store.getCount(many.depBead[manyName]), one.store.getCount(one.depBead[oneName]); got != want {
			t.Errorf("dependency %d bead was read %d times for 5 dependents, want the %d one dependent takes", j, got, want)
		}
	}

	t.Run("first filter", func(t *testing.T) {
		env := newDepProbeEnv(t, 5, 2)
		pass := env.pass(context.Background(), nil)
		for _, app := range env.dependents {
			if !env.allAlive(app, pass) {
				t.Fatalf("dependent %s should see its live dependencies", app)
			}
		}
		for _, dep := range env.deps {
			if got := env.sp.observationCount(dep); got != 1 {
				t.Errorf("dependency %s observed %d times in one first filter, want 1", dep, got)
			}
		}
	})
	t.Run("a new pass probes again", func(t *testing.T) {
		env := newDepProbeEnv(t, 1, 1)
		env.allAlive(env.dependents[0], env.pass(context.Background(), nil))
		env.allAlive(env.dependents[0], env.pass(context.Background(), nil))
		if got := env.sp.observationCount(env.deps[0]); got != 2 {
			t.Fatalf("dependency observed %d times over two passes, want 2: a recheck must see a change since the first filter", got)
		}
	})
}

// TestDependencyProbe_AnswerIsTheSameByIdAndByName pins that resolving the
// dependency by its bead id changes no answer: zero, one or two open beads for
// the name, with the runtime alive or dead. The expected values are main's.
func TestDependencyProbe_AnswerIsTheSameByIdAndByName(t *testing.T) {
	for _, tc := range []struct {
		name         string
		beads        int
		runtimeAlive bool
		want         bool
	}{
		{"no bead, runtime alive", 0, true, true},
		{"no bead, runtime dead", 0, false, false},
		{"one bead, runtime alive", 1, true, true},
		{"one bead, runtime dead", 1, false, false},
		{"two beads, runtime alive", 2, true, true},
		{"two beads, runtime dead", 2, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newDepProbeEnv(t, 1, 0)
			dep := fmt.Sprintf("dpsolo-%d", depProbeSeq.Add(1))
			env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: dep, MaxActiveSessions: intPtr(1)})
			env.desired[dep] = TemplateParams{TemplateName: dep, SessionName: dep}
			for range tc.beads {
				env.addBead(dep)
			}
			if tc.runtimeAlive {
				env.startRuntime(dep)
			}
			got := dependencyTemplateAlive(dep, env.cfg, env.desired, env.sp, "city", env.store, clock.Real{}, env.pass(context.Background(), nil))
			if got != tc.want {
				t.Fatalf("dependencyTemplateAlive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDependencyProbe_OneBeadResolvesByIdOtherwiseByName pins the store call
// log: with exactly one open bead for the dependency the probe reads that bead
// by id and never resolves the session name as an id; with zero or two it
// falls back to the by-name resolution.
func TestDependencyProbe_OneBeadResolvesByIdOtherwiseByName(t *testing.T) {
	for _, tc := range []struct {
		name       string
		beads      int
		wantByName bool
	}{
		{"zero beads falls back to by-name", 0, true},
		{"one bead resolves by id", 1, false},
		{"two beads fall back to by-name", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newDepProbeEnv(t, 1, 0)
			dep := fmt.Sprintf("dpsolo-%d", depProbeSeq.Add(1))
			env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: dep, MaxActiveSessions: intPtr(1)})
			env.desired[dep] = TemplateParams{TemplateName: dep, SessionName: dep}
			var ids []string
			for range tc.beads {
				ids = append(ids, env.addBead(dep))
			}
			env.startRuntime(dep)
			env.store.reset()
			dependencyTemplateAlive(dep, env.cfg, env.desired, env.sp, "city", env.store, clock.Real{}, env.pass(context.Background(), nil))

			if byName := env.store.getCount(dep) > 0; byName != tc.wantByName {
				t.Fatalf("store.Get(%q) by name seen = %v, want %v", dep, byName, tc.wantByName)
			}
			if tc.beads == 1 && env.store.getCount(ids[0]) == 0 {
				t.Fatalf("exactly one open bead %s, but the probe never read it by id", ids[0])
			}
		})
	}
}

// TestDependencyProbe_WedgedDependencyIsBoundedAndKeyed pins that a dependency
// whose liveness never answers holds the pass for no more than the bound,
// counts as not alive, is logged as unverifiable, and that the next pass finds
// the observation outstanding and answers at once without another provider call.
func TestDependencyProbe_WedgedDependencyIsBoundedAndKeyed(t *testing.T) {
	const bound = 150 * time.Millisecond
	shrinkDependencyProbeBound(t, bound)
	env := newDepProbeEnv(t, 1, 1)
	dep, app := env.deps[0], env.dependents[0]
	env.sp.wedgeSession(dep)

	var log bytes.Buffer
	alive, returned, took := runWithin(5*time.Second, func() bool { return env.allAlive(app, env.pass(context.Background(), &log)) })
	if !returned {
		t.Fatalf("the pass did not return within 5s of a wedged dependency: the probe is unbounded")
	}
	if alive {
		t.Fatal("a dependency that did not answer counted as alive")
	}
	if took > bound+time.Second {
		t.Fatalf("the pass took %v, want about the %v bound", took, bound)
	}
	if want := "dependency " + dep + " unverifiable"; !strings.Contains(log.String(), want) {
		t.Fatalf("log = %q, want it to say %q", log.String(), want)
	}
	if hasStartCall(env.sp.Calls, app) {
		t.Fatalf("the dependent %s was started against an unverified dependency", app)
	}
	if got := env.sp.observationCount(dep); got != 1 {
		t.Fatalf("wedged observation called %d times after the first pass, want 1", got)
	}

	alive, returned, took = runWithin(5*time.Second, func() bool { return env.allAlive(app, env.pass(context.Background(), nil)) })
	if !returned || alive {
		t.Fatalf("second pass returned=%v alive=%v, want an at-once not-alive answer", returned, alive)
	}
	if took > bound/2 {
		t.Fatalf("second pass took %v, want an at-once answer (outstanding key), well under the %v bound", took, bound)
	}
	if got := env.sp.observationCount(dep); got != 1 {
		t.Fatalf("second pass called the wedged observation again (%d calls), want it to stay at 1", got)
	}
}

// hasStartCall reports whether calls records a Start of name.
func hasStartCall(calls []runtime.Call, name string) bool {
	for _, c := range calls {
		if c.Method == "Start" && c.Name == name {
			return true
		}
	}
	return false
}

// TestDependencyProbe_UnansweredProbeIsMemoizedForThePass pins that a pass
// waits on a wedged dependency once however many dependents share it.
func TestDependencyProbe_UnansweredProbeIsMemoizedForThePass(t *testing.T) {
	const bound = 200 * time.Millisecond
	shrinkDependencyProbeBound(t, bound)
	env := newDepProbeEnv(t, 3, 1)
	env.sp.wedgeSession(env.deps[0])

	pass := env.pass(context.Background(), nil)
	_, returned, took := runWithin(5*time.Second, func() bool {
		for _, app := range env.dependents {
			if env.allAlive(app, pass) {
				return true
			}
		}
		return false
	})
	if !returned {
		t.Fatal("the pass did not return within 5s of a wedged dependency: the probe is unbounded")
	}
	if took > bound+bound/2 {
		t.Fatalf("three dependents of one wedged dependency took %v, want one %v bound", took, bound)
	}
	if got := env.sp.observationCount(env.deps[0]); got != 1 {
		t.Fatalf("wedged dependency observed %d times in one pass, want 1", got)
	}
}

// TestDependencyProbe_AbandonBudgetStopsAfterThreeConsecutiveWaits pins that
// five wedged dependency sessions cost a pass three bounds, and that the
// providers of the fourth and fifth receive no call.
func TestDependencyProbe_AbandonBudgetStopsAfterThreeConsecutiveWaits(t *testing.T) {
	const bound = 400 * time.Millisecond
	shrinkDependencyProbeBound(t, bound)
	env := newDepProbeEnv(t, 1, 5)
	for _, dep := range env.deps {
		env.sp.wedgeSession(dep)
	}

	_, returned, took := runWithin(10*time.Second, func() struct{} {
		env.waveOrder(env.pass(context.Background(), nil), env.dependents...)
		return struct{}{}
	})
	if !returned {
		t.Fatal("the pass did not return within 10s of five wedged dependencies: the probes are unbounded")
	}
	if took > 3*bound+bound/2 {
		t.Fatalf("the pass took %v, want at most three %v bounds", took, bound)
	}
	for j, dep := range env.deps {
		want := 1
		if j >= 3 {
			want = 0
		}
		if got := env.sp.callCount(dep); got != want {
			t.Errorf("wedged dependency %d received %d provider calls, want %d", j, got, want)
		}
	}
}

// TestDependencyProbe_AbandonBudgetNeedsConsecutiveWaits pins that a timely
// answer resets the count: wedged, answers, wedged, answers, wedged probes all
// five sessions.
func TestDependencyProbe_AbandonBudgetNeedsConsecutiveWaits(t *testing.T) {
	const bound = 150 * time.Millisecond
	shrinkDependencyProbeBound(t, bound)
	env := newDepProbeEnv(t, 1, 5)
	for _, j := range []int{0, 2, 4} {
		env.sp.wedgeSession(env.deps[j])
	}

	_, returned, _ := runWithin(10*time.Second, func() struct{} {
		env.waveOrder(env.pass(context.Background(), nil), env.dependents...)
		return struct{}{}
	})
	if !returned {
		t.Fatal("the pass did not return within 10s: the probes are unbounded")
	}
	for j, dep := range env.deps {
		if got := env.sp.observationCount(dep); got != 1 {
			t.Errorf("dependency %d observed %d times, want 1: a timely answer must reset the abandon count", j, got)
		}
	}
}

// TestDependencyProbe_OutstandingAnswerNeitherCountsNorResets pins that an
// observation already outstanding under the session's key answers at once and
// leaves the abandon count where it was: two abandoned waits, an outstanding
// answer, then a third abandoned wait trips the budget.
func TestDependencyProbe_OutstandingAnswerNeitherCountsNorResets(t *testing.T) {
	const bound = 150 * time.Millisecond
	shrinkDependencyProbeBound(t, bound)
	env := newDepProbeEnv(t, 1, 5)
	for _, j := range []int{0, 1, 3} {
		env.sp.wedgeSession(env.deps[j])
	}
	holdObservationOutstanding(t, env.cityPath, env.deps[2])

	_, returned, _ := runWithin(10*time.Second, func() struct{} {
		env.waveOrder(env.pass(context.Background(), nil), env.dependents...)
		return struct{}{}
	})
	if !returned {
		t.Fatal("the pass did not return within 10s: the probes are unbounded")
	}
	if got := env.sp.callCount(env.deps[2]); got != 0 {
		t.Errorf("dependency with an outstanding observation received %d provider calls, want 0", got)
	}
	if got := env.sp.callCount(env.deps[3]); got != 1 {
		t.Errorf("third abandoned wait received %d provider calls, want 1", got)
	}
	if got := env.sp.callCount(env.deps[4]); got != 0 {
		t.Errorf("session after the third abandoned wait received %d provider calls, want 0: the outstanding answer must not reset the count", got)
	}
}

// holdObservationOutstanding leaves an observation of sessionName in cityPath
// in flight under postStartObservationKey, started through the bounded-observer
// helper the start path uses, until the test ends.
func holdObservationOutstanding(t *testing.T, cityPath, sessionName string) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := observeSessionBounded(ctx, cityPath, sessionName, func() (bool, error) {
		<-release
		return true, nil
	})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("seeding an outstanding observation = %v, want an unanswered wait", err)
	}
	probe := func(context.Context) (bool, error) { return true, nil }
	if _, err := worker.ObserveBounded(context.Background(), postStartObservationKey(cityPath, sessionName), probe); !errors.Is(err, worker.ErrObservationOutstanding) {
		t.Fatalf("seeded observation is not outstanding: %v", err)
	}
}

// TestDependencyProbe_UsesTheCityPathKey pins that the dependency check shares
// the one-in-flight latch with every other bounded observer of the session: with
// an observation of S outstanding under postStartObservationKey(cityPath, S),
// the check answers not alive at once without a provider call. Main keyed the
// probe by "" + NUL + S and never saw the outstanding observation.
func TestDependencyProbe_UsesTheCityPathKey(t *testing.T) {
	shrinkDependencyProbeBound(t, time.Second)
	env := newDepProbeEnv(t, 1, 1)
	dep, app := env.deps[0], env.dependents[0]
	holdObservationOutstanding(t, env.cityPath, dep)

	alive, returned, _ := runWithin(5*time.Second, func() bool { return env.allAlive(app, env.pass(context.Background(), nil)) })
	if !returned {
		t.Fatal("the dependency check did not return within 5s")
	}
	if alive {
		t.Fatal("dependency counted as alive while an observation of it was outstanding")
	}
	if got := env.sp.callCount(dep); got != 0 {
		t.Fatalf("provider received %d calls for a session with an outstanding observation, want 0", got)
	}
}
