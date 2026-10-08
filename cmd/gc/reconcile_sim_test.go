package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The deterministic simulator (D1a; CONTRACT v5 §10, §11). It runs the real
// pass (gather, allocate, decideRow, admit, submit) on a fake clock over two
// census legs, each a real CachingStore over a stamped, revisioned MemStore;
// the real inventory lane over simProvider, a fake tmux backend; and the real
// executor, whose effects park until a step releases them. A seeded RNG picks
// each step, and the invariants are checked after it against ground truth
// (the backing stores and the fake runtime). A failing seed prints its
// schedule; GC_V2_SIM_SEED replays it. Hooks for later PRs: whatever
// effectRegistry holds runs here unchanged, files add invariants through the
// sim*Checks hooks, and scenarios drive sim's steps directly.

const (
	simPatrol  = 10 * time.Second
	simSteps   = 500
	simCISeeds = 64
	simGuard   = 10 * time.Second // real time: turns a hang into a failure
	simCityLeg = "city"
	simRigLeg  = "rig-a"
)

// simStaleEvents lets a step deliver an event older than its row's backing:
// a reorder, or a duplicate after a newer write (v5 §11 H2).
// GC_V2_SIM_NO_STALE_EVENTS=1 turns it off.
var simStaleEvents = os.Getenv("GC_V2_SIM_NO_STALE_EVENTS") == ""

// The invariant hooks a later file appends to: per step, per row change v2
// made, around each effect (the returned func sees its settlement), and at
// quiescence.
var (
	simStepChecks   []func(*sim)
	simWriteChecks  []func(*sim, simWrite)
	simEffectChecks []func(*sim, *simEffect) func(settlement)
	simQuietChecks  []func(*sim)
)

// simRuntime is one runtime under a name.
type simRuntime struct {
	id, epoch, token string // GC_SESSION_ID, GC_RUNTIME_EPOCH, GC_INSTANCE_TOKEN
	object, created  int64  // tmux #{session_id} and #{session_created}
	corpse           bool   // the pane died; remain-on-exit keeps the name listed
	zombie           bool   // the pane lives; the agent died
	attached         bool
	probeErr         bool // a process probe errors, answering the agent not alive (v5 O3)
}

// simStart and simKill are the spy's records of a Start and of a
// destructive call, with the runtime it hit.
type simStart struct {
	Name, ID, Token              string
	FreshOnly, External, Created bool
}

type simKill struct {
	Method, Name string
	Hit          *simRuntime
	External     bool
}

var errSimListing = errors.New("sim: list-sessions failed")

// simProvider is a fake tmux backend over runtime.Fake, whose other knobs
// (peek, pending) it keeps: it lists corpses, keeps identity env per runtime,
// answers every read fresh (so runtime.ObserveLivenessSince needs no method)
// with LL2's Present bit and object ids, and records every Start and
// destructive call.
type simProvider struct {
	*runtime.Fake
	mu          sync.Mutex
	rts         map[string]*simRuntime
	version     uint64            // bumped by every runtime change
	changed     map[string]uint64 // each name's version at its runtime's last change
	objects     int64
	now         func() time.Time
	listing     int // 0 complete; 1 partial, the first name missing; 2 failed (v5 O1)
	serverDown  bool
	confirmable bool  // a down server confirms dead (LL2)
	external    bool  // the calls now are an operator's or a second controller's
	nextStart   error // the next Start's scripted outcome
	starts      []simStart
	kills       []simKill
}

// put places a runtime under name; drop removes it. Both run under mu.
func (p *simProvider) put(name, id, epoch, token string) {
	p.objects++
	p.rts[name] = &simRuntime{id: id, epoch: epoch, token: token, object: p.objects, created: p.now().Unix()}
	p.serverDown = false
	p.touch(name)
}

func (p *simProvider) drop(name string) {
	delete(p.rts, name)
	p.touch(name)
}

func (p *simProvider) touch(name string) {
	p.version++
	p.changed[name] = p.version
}

func (p *simProvider) ListRunning(string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.serverDown {
		return nil, &runtime.PartialListError{Err: errSimListing, ServerAbsent: true}
	}
	names := slices.Sorted(maps.Keys(p.rts))
	switch p.listing {
	case 1:
		return names[min(1, len(names)):], &runtime.PartialListError{Err: errSimListing}
	case 2:
		return nil, errSimListing
	}
	return names, nil
}

func (p *simProvider) ListRunningComplete() bool { return true }

func (p *simProvider) ServerConfirmedDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.serverDown && p.confirmable
}

func (p *simProvider) RuntimeInventory(context.Context) (map[string]runtime.InventoryEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]runtime.InventoryEntry, len(p.rts))
	for name, rt := range p.rts {
		inc := fmt.Sprintf("$%d:%d:%d", rt.object, rt.created, rt.object+1000)
		out[name] = runtime.InventoryEntry{Incarnation: inc, DeadKnown: true, AllPanesDead: rt.corpse, AttachedKnown: true, Attached: rt.attached}
	}
	return out, nil
}

func (p *simProvider) GetAllEnvironment(name string) (map[string]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rts[name]
	if rt == nil {
		return nil, fmt.Errorf("sim: no session %s", name)
	}
	return map[string]string{"GC_SESSION_ID": rt.id, "GC_RUNTIME_EPOCH": rt.epoch, "GC_INSTANCE_TOKEN": rt.token, "GT_PROCESS_NAMES": "agent"}, nil
}

func (p *simProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt := p.rts[name]; rt != nil && rt.probeErr && !rt.corpse {
		l := p.livenessLocked(name)
		l.Alive = false
		return l, fmt.Errorf("sim: probe of %s: %w", name, runtime.ErrRuntimeUnavailable)
	}
	return p.livenessLocked(name), nil
}

func (p *simProvider) livenessLocked(name string) runtime.Liveness {
	rt := p.rts[name]
	if rt == nil {
		return runtime.Liveness{}
	}
	l := runtime.Liveness{ObjectID: fmt.Sprintf("$%d", rt.object), ObjectCreated: strconv.FormatInt(rt.created, 10), Corpse: rt.corpse}
	if !rt.corpse {
		l.Running, l.Alive, l.PanePID = true, !rt.zombie, strconv.FormatInt(rt.object+1000, 10)
	}
	return l
}

func (p *simProvider) IsAttached(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rts[name] != nil && p.rts[name].attached
}

// Start creates a runtime carrying cfg's identity env, unless a scripted
// outcome fails it or the name is held (FreshOnly's ErrSessionExists).
func (p *simProvider) Start(_ context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := simStart{Name: name, ID: cfg.Env["GC_SESSION_ID"], Token: cfg.Env["GC_INSTANCE_TOKEN"], FreshOnly: cfg.FreshOnly, External: p.external}
	err := p.nextStart
	p.nextStart = nil
	if err == nil && p.rts[name] != nil {
		err = runtime.ErrSessionExists
	}
	if s.Created = err == nil; s.Created {
		p.put(name, s.ID, cfg.Env["GC_RUNTIME_EPOCH"], s.Token)
	}
	p.starts = append(p.starts, s)
	return err
}

func (p *simProvider) Stop(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killLocked("Stop", name)
	return nil
}

func (p *simProvider) killLocked(method, name string) {
	var hit *simRuntime
	if rt := p.rts[name]; rt != nil {
		c := *rt
		hit = &c
		p.drop(name)
	}
	p.kills = append(p.kills, simKill{Method: method, Name: name, Hit: hit, External: p.external})
}

// simBacking is one leg's revisioned store: a MemStore stamped require, whose
// reads the cache serves exactly, as the controller's SQLite binding.
type simBacking struct{ *beads.MemStore }

func (simBacking) CachedReadExact() bool { return true }

// simLeg is one census leg: its backing, the cache the controller reads and
// writes through, and the outside writers' events, which a step delivers.
type simLeg struct {
	name    string
	backing *beads.MemStore
	cache   *beads.CachingStore
	events  []json.RawMessage
}

// simEffect is one submitted effect, parked until a step releases it.
type simEffect struct {
	it      intent
	release chan struct{}
	done    chan settlement
	settled bool // its settlement was posted: it ran, or its deadline passed
}

// simWrite is one row change between two steps, by its actor.
type simWrite struct {
	Leg, Actor    string // Actor: "v2", or the outside step's name
	Before, After beads.Bead
	At            time.Time
}

// heldListing is an inventory listing not yet published (inventory lag).
type heldListing struct {
	listing inventoryListing
	started time.Time
	version uint64 // the provider's when listed
	passes  int    // planner passes until it publishes
}

// sim is one seeded run.
type sim struct {
	t        *testing.T
	seed     uint64
	rng      *rand.Rand
	lag      bool
	schedule []string
	clk      *fakePlannerClock
	obsClk   *clock.Fake
	cfg      *config.City
	sp       *simProvider
	legs     []*simLeg
	env      gatherEnv
	p        *planner
	inflight *inflightMap
	x        *effectExecutor
	lane     *runtimeInventoryLane
	held     []heldListing
	listed   uint64 // the provider version the latest published listing saw
	spawned  sync.WaitGroup
	parkCh   chan *simEffect
	postCh   chan settlement
	posted   []settlement // posts not yet awaited
	parked   []*simEffect
	lastSeq  uint64
	prev     map[string]beads.Bead // "leg/id" → row, after the last step
	writes   []simWrite
	admitted []time.Time // admitted starts (I9)
	failures []string
}

// simOpts scripts a run: rows replaces the seeded rows, and the other fields
// seed one fault into the code under test through a test seam.
type simOpts struct {
	rows     func(s *sim) (city, rig []beads.Bead)
	inflight func(*inflightMap) plannerInflight
	registry func(map[string]effectBuilder) map[string]effectBuilder
	arms     func([]rowArm) []rowArm
}

func newSim(t *testing.T, seed uint64, o simOpts) *sim {
	s := &sim{
		t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0x51d1a)), clk: newFakePlannerClock(plannerT0), obsClk: &clock.Fake{Time: plannerT0},
		parkCh: make(chan *simEffect, 256), postCh: make(chan settlement, 256),
	}
	s.lag = s.rng.IntN(2) == 0
	s.sp = &simProvider{Fake: runtime.NewFake(), rts: make(map[string]*simRuntime), changed: make(map[string]uint64), now: s.clk.Now}
	s.cfg = workerCity(3)
	s.cfg.Daemon.PatrolInterval, s.cfg.Daemon.ProbeConcurrency = simPatrol.String(), intPtr(1)
	s.cfg.Rigs = []config.Rig{{Name: simRigLeg, Path: t.TempDir()}}

	if o.rows == nil {
		o.rows = (*sim).seedRows
	}
	city, rig := o.rows(s)
	for name, rows := range map[string][]beads.Bead{simCityLeg: city, simRigLeg: rig} {
		m := beads.NewMemStoreFrom(0, rows, nil)
		if err := beads.StampOpenedStore(m, "MemStore", gate.Require, nil, nil); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		cache := beads.NewCachingStoreForTest(simBacking{m}, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("prime %s: %v", name, err)
		}
		s.legs = append(s.legs, &simLeg{name: name, backing: m, cache: cache})
	}
	slices.SortFunc(s.legs, func(a, b *simLeg) int { return strings.Compare(a.name, b.name) }) // city, then rig-a

	s.lane = newRuntimeInventoryLane(simPatrol, io.Discard, "sim")
	s.lane.clock, s.lane.cache = s.obsClk, NewObservationCache(s.obsClk, 2*simPatrol, "sim")
	env := &reconcileEnv{Gen: 1, Cfg: s.cfg, SP: s.sp}
	s.env = gatherEnv{
		CityPath: t.TempDir(), CityName: "test-city",
		Env:          func() *reconcileEnv { return env },
		Sessions:     func() beads.Store { return s.legs[0].cache },
		RigStores:    func() map[string]beads.Store { return map[string]beads.Store{simRigLeg: s.legs[1].cache} },
		Recording:    func() *externalReadsRecording { return nil },
		Observations: func() *ObservationCache { return s.lane.cache },
		ResolveTemplate: func(_ *reconcileEnv, info session.Info) (TemplateParams, error) {
			return TemplateParams{SessionName: info.SessionNameMetadata}, nil
		},
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
	}

	s.inflight = newInflightMap()
	var inflight plannerInflight = s.inflight
	if o.inflight != nil {
		inflight = o.inflight(s.inflight)
	}
	// The executor in step mode: its goroutines are joined at teardown, and
	// every effect parks (gated) until a step releases it.
	s.x = newEffectExecutor(func(st settlement) { s.p.settlements.post(st); s.postCh <- st }, io.Discard)
	s.x.clock = s.clk
	s.x.spawn = func(f func()) {
		s.spawned.Add(1)
		go func() {
			defer s.spawned.Done()
			f()
		}()
	}
	s.p = newPlanner(s.clk, func() time.Duration { return simPatrol }, nil, inflight, nil, io.Discard)
	s.p.pass = func(now time.Time) passResult { return s.p.tracePass(s.env, now) }
	s.p.effects = s.x
	reg := effectRegistry
	if o.registry != nil {
		reg = o.registry(reg)
	}
	withRegistry(t, s.gated(reg))
	if o.arms != nil {
		saved := rowArms
		rowArms = o.arms(slices.Clone(saved))
		t.Cleanup(func() { rowArms = saved })
	}
	t.Cleanup(s.teardown)
	s.prev = s.snapshot()
	return s
}

// seedRows draws pool rows in several states, some with a timer or in a
// state this version does not know, the live ones with their runtime.
func (s *sim) seedRows() (city, rig []beads.Bead) {
	row := func(id string, slot int) beads.Bead {
		gen := 1 + s.rng.IntN(3)
		token := fmt.Sprintf("tok-%s-%d", id, gen)
		state := []string{"asleep", "active", "awake", "creating", "asleep"}[s.rng.IntN(5)]
		meta := []string{"generation", strconv.Itoa(gen), "instance_token", token}
		switch at := s.rel(time.Duration(s.rng.IntN(240)-120) * time.Second); s.rng.IntN(6) {
		case 0:
			meta = append(meta, "held_until", at, "sleep_reason", "user-hold")
		case 1:
			meta = append(meta, "quarantined_until", at, "sleep_reason", "quarantine")
		case 2:
			state = "archived-v9"
		}
		if state == "active" || state == "awake" {
			s.sp.put("s-"+id, id, strconv.Itoa(gen), token)
		}
		return poolRow(id, "worker", slot, state, meta...)
	}
	for i := 1; i <= 3+s.rng.IntN(4); i++ {
		city = append(city, row(fmt.Sprintf("gc-%d", i), i))
	}
	city = append(city, routedDemandBead("gc-w1"), routedDemandBead("gc-w2"))
	for i := 1; i <= 1+s.rng.IntN(2); i++ {
		rig = append(rig, row(fmt.Sprintf("rg-%d", i), 10+i))
	}
	return city, rig
}

func (s *sim) rel(d time.Duration) string { return s.clk.Now().Add(d).UTC().Format(time.RFC3339) }

// gated wraps every effect so it parks until a step releases it.
func (s *sim) gated(reg map[string]effectBuilder) map[string]effectBuilder {
	out := make(map[string]effectBuilder, len(reg))
	for kind, build := range reg {
		out[kind] = func(p *effectPass, it intent) func(context.Context) settlement {
			run := build(p, it)
			return func(ctx context.Context) settlement {
				e := &simEffect{it: it, release: make(chan struct{}), done: make(chan settlement, 1)}
				s.parkCh <- e
				<-e.release
				res := run(ctx)
				e.done <- res
				return res
			}
		}
	}
	return out
}

func (s *sim) logf(format string, args ...any) {
	s.schedule = append(s.schedule, fmt.Sprintf("%3d %6s ", len(s.schedule), s.clk.Now().Sub(plannerT0))+fmt.Sprintf(format, args...))
}

// pass runs one pass, waits for each effect it submitted to park, and
// publishes the held listings whose lag ran out.
func (s *sim) pass() {
	now := s.clk.Now()
	s.obsClk.Time = now
	s.p.runPass(now)
	rec := s.p.out.record.Load()
	s.logf("pass: admitted %v, %d deferred %s", intentKeys(rec.Admitted), len(rec.Deferred), rec.Err)
	for _, it := range rec.Admitted {
		if it.Kind == intentStart {
			s.admitted = append(s.admitted, now)
		}
	}
	for _, e := range s.inflight.view().Entries {
		if e.Seq <= s.lastSeq || e.Ambiguous {
			continue
		}
		select {
		case p := <-s.parkCh:
			s.parked = append(s.parked, p)
		case <-time.After(simGuard):
			s.t.Fatalf("seed %d: a submitted effect never parked", s.seed)
		}
	}
	s.lastSeq = s.inflight.seq
	slices.SortFunc(s.parked, func(a, b *simEffect) int { return strings.Compare(a.it.Key.Leg+a.it.Key.ID, b.it.Key.Leg+b.it.Key.ID) })
	for i := 0; i < len(s.held); i++ {
		if s.held[i].passes--; s.held[i].passes < 0 {
			s.publish(s.held[i])
			s.held = slices.Delete(s.held, i, i+1)
			i--
		}
	}
}

// release runs parked effect i to its end and waits for its settlement, or,
// when its deadline already settled it, for its late event.
func (s *sim) release(i int) {
	e := s.parked[i]
	s.parked = slices.Delete(s.parked, i, i+1)
	var after []func(settlement)
	for _, c := range simEffectChecks {
		after = append(after, c(s, e))
	}
	close(e.release)
	var res settlement
	select {
	case res = <-e.done:
	case <-time.After(simGuard):
		s.t.Fatalf("seed %d: effect %s %v never returned", s.seed, e.it.Kind, e.it.Key)
	}
	s.logf("effect %s %s: outcome %d %s (late %t)", e.it.Kind, e.it.Key.ID, res.Outcome, res.Cause, e.settled)
	for _, c := range after {
		c(res)
	}
	switch {
	case !e.settled:
		s.awaitPost(func(st settlement) bool { return st.Key == e.it.Key && st.Kind == e.it.Kind })
	case res.Event != nil:
		s.awaitPost(func(st settlement) bool { return st.Key == (rowKey{}) && st.Event != nil })
	}
}

// awaitPost waits for the executor's post that match picks.
func (s *sim) awaitPost(match func(settlement) bool) {
	for {
		if i := slices.IndexFunc(s.posted, match); i >= 0 {
			s.posted = slices.Delete(s.posted, i, i+1)
			return
		}
		select {
		case st := <-s.postCh:
			s.posted = append(s.posted, st)
		case <-time.After(simGuard):
			s.t.Fatalf("seed %d: a settlement was never posted", s.seed)
		}
	}
}

// advance moves the clock, and waits for the executor to settle each parked
// effect whose deadline it passed.
func (s *sim) advance(d time.Duration) {
	s.clk.Advance(d)
	s.obsClk.Time = s.clk.Now()
	s.logf("advance %s", d)
	for _, e := range s.parked {
		if !e.settled && !e.it.Deadline.After(s.clk.Now()) {
			s.awaitPost(func(st settlement) bool { return st.Key == e.it.Key && st.Kind == e.it.Kind })
			e.settled = true
		}
	}
}

// inventory lists the runtime and publishes the pass, or in the
// inventory-lag mode may hold it for up to three planner passes, so it
// publishes what the runtime was.
func (s *sim) inventory() {
	l := s.lane
	s.obsClk.Time = s.clk.Now()
	l.seq++
	if !sameInventoryProvider(s.sp, l.lastProvider) {
		l.lastProvider, l.providerGen = s.sp, l.providerGen+1
	}
	listing, _ := l.listBounded(context.Background(), s.sp) // the fake answers at once
	h := heldListing{listing: listing, started: s.clk.Now(), version: s.sp.version}
	if s.lag && s.rng.IntN(2) == 0 {
		h.passes = s.rng.IntN(4)
		s.held = append(s.held, h)
		s.logf("inventory: listed %v, held %d passes", listing.mergedNames, h.passes)
		return
	}
	s.publish(h)
}

func (s *sim) publish(h heldListing) {
	s.listed = h.version
	s.lane.publish(context.Background(), h.listing, h.started, 1, &inventoryPassReport{})
	s.logf("inventory: published %v (%v), listed at %s", h.listing.mergedNames, h.listing.mergedErr, h.started.Sub(plannerT0))
}

// external runs fn as an outside writer (the CLI, an agent, a second
// controller) on a random row of a random leg, writing its backing, and
// queues each changed row's event.
func (s *sim) external(name string, fn func(l *simLeg, b beads.Bead)) {
	l := s.legs[s.rng.IntN(len(s.legs))]
	rows, _ := l.backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	rows = slices.DeleteFunc(rows, func(b beads.Bead) bool { return b.Metadata["session_name"] == "" })
	if len(rows) == 0 {
		return
	}
	b := rows[s.rng.IntN(len(rows))]
	s.sp.external = true // no effect runs while the sim steps
	fn(l, b)
	s.sp.external = false
	s.logf("external %s on %s/%s", name, l.name, b.ID)
	s.audit(name)
}

func (s *sim) setMeta(l *simLeg, id string, kv ...string) {
	m := make(map[string]string)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	if err := l.backing.SetMetadataBatch(id, m); err != nil {
		s.t.Fatalf("seed %d: outside write: %v", s.seed, err)
	}
}

// externalOps are the outside writers' moves on a row.
var externalOps = []struct {
	name string
	fn   func(s *sim, l *simLeg, b beads.Bead)
}{
	{"hold", func(s *sim, l *simLeg, b beads.Bead) {
		s.setMeta(l, b.ID, "held_until", s.rel(time.Duration(1+s.rng.IntN(120))*time.Second), "sleep_reason", "user-hold")
	}},
	{"quarantine", func(s *sim, l *simLeg, b beads.Bead) {
		s.setMeta(l, b.ID, "quarantined_until", s.rel(time.Duration(1+s.rng.IntN(120))*time.Second), "sleep_reason", "quarantine")
	}},
	{"kill", func(s *sim, l *simLeg, b beads.Bead) { // gc session kill: the fence, then the stop
		if err := l.backing.SetMetadataBatch(b.ID, session.KillPendingPatch(s.clk.Now())); err != nil {
			s.t.Fatal(err)
		}
		_ = s.sp.Stop(b.Metadata["session_name"])
	}},
	{"unknown-state", func(s *sim, l *simLeg, b beads.Bead) { s.setMeta(l, b.ID, "state", "archived-v9") }},
	{"asleep", func(s *sim, l *simLeg, b beads.Bead) { s.setMeta(l, b.ID, "state", "asleep", "state_reason", "") }},
	{"second-controller-prewake", func(s *sim, l *simLeg, b beads.Bead) {
		gen, _ := strconv.Atoi(b.Metadata["generation"])
		token := fmt.Sprintf("tok-%s-%d", b.ID, gen+1)
		s.setMeta(l, b.ID, "generation", strconv.Itoa(gen+1), "instance_token", token, "state", "awake")
		_ = s.sp.Stop(b.Metadata["session_name"])
		_ = s.sp.Start(context.Background(), b.Metadata["session_name"], runtime.Config{Env: map[string]string{"GC_SESSION_ID": b.ID, "GC_RUNTIME_EPOCH": strconv.Itoa(gen + 1), "GC_INSTANCE_TOKEN": token}})
	}},
	{"drain-ack", func(s *sim, l *simLeg, b beads.Bead) { // E3's CAS, from the agent's pane
		if commit, err := checkDrainAckRow(l.backing, b.ID, false, b.Metadata["instance_token"], s.clk.Now()); err == nil {
			_ = commit()
		}
	}},
	{"close", func(_ *sim, l *simLeg, b beads.Bead) { _ = l.backing.Close(b.ID) }},
	{"suspend", func(s *sim, l *simLeg, b beads.Bead) { // gc session suspend: the stop, then the state
		_ = s.sp.Stop(b.Metadata["session_name"])
		s.setMeta(l, b.ID, "state", string(session.StateSuspended), "suspended_at", s.rel(0), "slept_at", "", "sleep_reason", "")
	}},
}

// runtimeOps change the runtime under a row's name, under the provider's mu.
var runtimeOps = []struct {
	name string
	fn   func(p *simProvider, name string, b beads.Bead)
}{
	{"agent-dies", func(p *simProvider, name string, _ beads.Bead) {
		if rt := p.rts[name]; rt != nil && !rt.zombie {
			rt.zombie = true
			p.touch(name)
		}
	}},
	{"pane-dies", func(p *simProvider, name string, _ beads.Bead) {
		if rt := p.rts[name]; rt != nil && !rt.corpse {
			rt.corpse = true
			p.touch(name)
		}
	}},
	{"vanish", func(p *simProvider, name string, _ beads.Bead) { p.drop(name) }},
	{"probe-errs", func(p *simProvider, name string, _ beads.Bead) {
		if rt := p.rts[name]; rt != nil {
			rt.probeErr = !rt.probeErr
			p.touch(name)
		}
	}},
	{"foreign-occupant", func(p *simProvider, name string, _ beads.Bead) { p.put(name, "gc-other", "1", "tok-other") }},
	{"ownerless-occupant", func(p *simProvider, name string, _ beads.Bead) { p.put(name, "", "", "") }},
	{"attach", func(p *simProvider, name string, _ beads.Bead) {
		if rt := p.rts[name]; rt != nil {
			rt.attached = !rt.attached
		}
	}},
	{"attach-recreates", func(p *simProvider, name string, b beads.Bead) { // gc session attach, on the row's token
		p.put(name, b.ID, b.Metadata["generation"], b.Metadata["instance_token"])
		p.starts = append(p.starts, simStart{Name: name, ID: b.ID, Token: b.Metadata["instance_token"], External: true, Created: true})
	}},
	{"server-dies", func(p *simProvider, _ string, _ beads.Bead) {
		for name := range p.rts {
			p.drop(name)
		}
		p.serverDown, p.confirmable = true, p.objects%2 == 0
	}},
	{"listing", func(p *simProvider, _ string, _ beads.Bead) { p.listing = (p.listing + 1) % 3 }},
}

// deliver applies, duplicates or drops one held event on a leg, picked at
// random (a reorder), or rescans a cache (its periodic reconcile, R-6).
func (s *sim) deliver() {
	l := s.legs[s.rng.IntN(len(s.legs))]
	if len(l.events) == 0 || s.rng.IntN(8) == 0 {
		l.cache.ReconcileNowForTest()
		s.logf("rescan %s", l.name)
		return
	}
	i := s.rng.IntN(len(l.events))
	ev, stale := l.events[i], s.stale(l, l.events[i])
	switch op := s.rng.IntN(6); {
	case op == 0 || (stale && !simStaleEvents):
		s.logf("drop event %d on %s (stale %t)", i, l.name, stale)
	case op == 1:
		l.cache.ApplyEvent("bead.updated", ev)
		s.logf("duplicate event %d on %s (stale %t)", i, l.name, stale)
		return
	default:
		l.cache.ApplyEvent("bead.updated", ev)
		s.logf("deliver event %d on %s (stale %t)", i, l.name, stale)
	}
	l.events = slices.Delete(l.events, i, i+1)
}

// stale reports whether ev's row has changed on l's backing since ev.
func (s *sim) stale(l *simLeg, ev json.RawMessage) bool {
	var b beads.Bead
	if err := json.Unmarshal(ev, &b); err != nil {
		s.t.Fatal(err)
	}
	cur, err := l.backing.Get(b.ID)
	return err == nil && !sameBead(b, cur)
}

// step runs one step the RNG picks, then audits it.
func (s *sim) step() {
	switch r := s.rng.IntN(100); {
	case r < 22:
		s.pass()
	case r < 36:
		if len(s.parked) > 0 {
			s.release(s.rng.IntN(len(s.parked)))
		}
	case r < 46:
		s.advance(time.Duration(1+s.rng.IntN(int(simPatrol/time.Second))) * time.Second)
	case r < 56:
		s.inventory()
	case r < 70:
		s.deliver()
	case r < 84:
		op := externalOps[s.rng.IntN(len(externalOps))]
		s.external(op.name, func(l *simLeg, b beads.Bead) { op.fn(s, l, b) })
		return
	default:
		op := runtimeOps[s.rng.IntN(len(runtimeOps))]
		s.external("runtime "+op.name, func(_ *simLeg, b beads.Bead) {
			s.sp.mu.Lock()
			defer s.sp.mu.Unlock()
			op.fn(s.sp, b.Metadata["session_name"], b)
		})
		return
	}
	s.audit("v2")
}

// audit attributes every row change since the last step to actor, queues an
// outside writer's events, and checks the invariants.
func (s *sim) audit(actor string) {
	next := s.snapshot()
	for _, k := range slices.Sorted(maps.Keys(next)) {
		after, before := next[k], s.prev[k]
		if before.ID != "" && sameBead(before, after) {
			continue
		}
		leg, _, _ := strings.Cut(k, "/")
		s.writes = append(s.writes, simWrite{Leg: leg, Actor: actor, Before: before, After: after, At: s.clk.Now()})
		if actor == "v2" {
			continue
		}
		payload, err := json.Marshal(after)
		if err != nil {
			s.t.Fatal(err)
		}
		for _, l := range s.legs {
			if l.name == leg {
				l.events = append(l.events, payload)
			}
		}
	}
	s.check()
	s.prev = next
}

func sameBead(a, b beads.Bead) bool {
	return a.Status == b.Status && maps.Equal(a.Metadata, b.Metadata)
}

func (s *sim) snapshot() map[string]beads.Bead {
	out := make(map[string]beads.Bead)
	for _, l := range s.legs {
		rows, err := l.backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if err != nil {
			s.t.Fatal(err)
		}
		for _, b := range rows {
			b.Metadata = maps.Clone(b.Metadata)
			out[l.name+"/"+b.ID] = b
		}
	}
	return out
}

// run runs steps until a violation, then quiesces.
func (s *sim) run(steps int) {
	s.inventory()
	for range steps {
		if s.step(); len(s.failures) > 0 {
			return
		}
	}
	s.quiesce()
}

// teardown cancels every effect, releases the parked ones and joins the
// executor's goroutines.
func (s *sim) teardown() {
	s.x.close()
	s.x.cancel()
	for _, e := range s.parked {
		close(e.release)
	}
	done := make(chan struct{})
	go func() { s.spawned.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(simGuard):
		s.t.Errorf("seed %d: executor goroutines still running at teardown", s.seed)
	}
}

// failf records an invariant violation once; inv leads with its I-number.
func (s *sim) failf(inv, format string, args ...any) {
	msg := inv + ": " + fmt.Sprintf(format, args...)
	if !slices.Contains(s.failures, msg) {
		s.failures = append(s.failures, msg)
		s.logf("VIOLATION %s", msg)
	}
}

// report fails t with the seed's violations and its schedule.
func (s *sim) report(t *testing.T) {
	t.Helper()
	if len(s.failures) > 0 {
		t.Errorf("seed %d (inventory lag %t) violated:\n  %s\nreplay: GC_V2_SIM_SEED=%d go test -run TestSimCorpus ./cmd/gc/\nschedule:\n%s",
			s.seed, s.lag, strings.Join(s.failures, "\n  "), s.seed, strings.Join(s.schedule, "\n"))
	}
}

// The invariants (CONTRACT v5 §10), checked after every step against the
// backing stores and the fake runtime: I8 and I10 here, the rest through the
// hooks. Each failure leads with its I-number.

func (s *sim) check() {
	for _, w := range s.writes {
		for _, c := range simWriteChecks {
			if w.Actor == "v2" {
				c(s, w)
			}
		}
	}
	s.writes = nil
	s.checkInflight()
	for _, c := range simStepChecks {
		c(s)
	}
}

// checkInflight is I8 (I-inflight, P5): the running entries mirror the
// executor (an entry whose settlement is posted but not yet drained
// included), and an ambiguous create clears by the hard bound.
func (s *sim) checkInflight() {
	queued := make(map[rowKey]bool)
	s.p.settlements.mu.Lock()
	for _, st := range s.p.settlements.items {
		queued[st.Key] = true
	}
	s.p.settlements.mu.Unlock()
	s.x.mu.Lock()
	exec := maps.Clone(s.x.inflight)
	s.x.mu.Unlock()
	for k := range s.inflight.running {
		if exec[effectKey{row: k}] == nil && !queued[k] {
			s.failf("I8 I-inflight", "the in-flight entry for %v outlives its effect's settlement", k)
		}
	}
	for k := range exec {
		if k.token != "" {
			if _, ok := s.inflight.creates[k.token]; !ok {
				s.failf("I8 I-inflight", "the executor runs a create %s with no in-flight entry", k.token)
			}
			continue
		}
		if _, ok := s.inflight.running[k.row]; !ok && k.row != (rowKey{}) {
			s.failf("I8 I-inflight", "the executor runs an effect for %v with no in-flight entry", k.row)
		}
	}
	for _, e := range s.inflight.creates {
		if e.Ambiguous && s.clk.Now().Sub(e.SettledAt) > inflightHardBound+2*simPatrol {
			s.failf("I8 I-inflight", "ambiguous create %s outlived the %s hard bound", e.Token, inflightHardBound)
		}
	}
}

// quiesce holds the inputs stable: every held event and listing is
// delivered, then a pass runs each patrol, every effect released at once,
// until 2 × patrol + startup_timeout past the last pending timer. From
// there, two passes in a row must admit nothing and change nothing (I10),
// and the in-flight map must then be empty (I8).
func (s *sim) quiesce() {
	s.logf("quiesce")
	for _, l := range s.legs {
		for _, ev := range l.events {
			if simStaleEvents || !s.stale(l, ev) {
				l.cache.ApplyEvent("bead.updated", ev)
			}
		}
		l.events = nil
		l.cache.ReconcileNowForTest()
	}
	for _, h := range s.held {
		s.publish(h)
	}
	s.held, s.lag = nil, false
	for len(s.parked) > 0 {
		s.release(0)
	}
	s.audit("v2")
	slack := 2*simPatrol + s.cfg.Session.StartupTimeoutDuration()
	bound := s.lastTimer().Add(slack)
	for quiet := 0; quiet < 2 && len(s.failures) == 0; {
		before := s.snapshot()
		s.inventory()
		s.pass()
		admitted := intentKeys(s.p.out.record.Load().Admitted)
		for len(s.parked) > 0 {
			s.release(0)
		}
		s.audit("v2")
		changed := !maps.EqualFunc(before, s.snapshot(), sameBead)
		switch {
		case s.clk.Now().Before(bound):
		case len(admitted) > 0 || changed:
			s.failf("I10 I-bounded", "no fixed point %s after the last timer: admitted %v, store changed %t", s.clk.Now().Sub(bound)+slack, admitted, changed)
		default:
			quiet++
		}
		s.advance(simPatrol)
	}
	for _, c := range simQuietChecks {
		c(s)
	}
	if n := len(s.inflight.view().Entries); n > 0 && len(s.failures) == 0 {
		s.failf("I8 I-inflight", "%d entries in flight at quiescence", n)
	}
}

// lastTimer is the latest pending timer: a row's hold, quarantine or kill
// fence, or a backoff's expiry.
func (s *sim) lastTimer() time.Time {
	last := s.clk.Now()
	later := func(t time.Time) {
		if t.After(last) {
			last = t
		}
	}
	for _, b := range s.snapshot() {
		for k, grace := range map[string]time.Duration{"held_until": 0, "quarantined_until": 0, "slept_at": session.KillPendingGrace} {
			if at, err := time.Parse(time.RFC3339, b.Metadata[k]); err == nil {
				later(at.Add(grace))
			}
		}
	}
	for _, r := range s.p.backoff.Snapshot() {
		later(r.Until)
	}
	return last
}

// simSeeds is the corpus: GC_V2_SIM_SEED replays one seed, GC_V2_SIM_SEEDS
// runs seeds 1..N (nightly), and CI runs simCISeeds.
func simSeeds(t *testing.T) []uint64 {
	first, n := uint64(1), uint64(simCISeeds)
	for env, v := range map[string]*uint64{"GC_V2_SIM_SEED": &first, "GC_V2_SIM_SEEDS": &n} {
		var err error
		if raw := os.Getenv(env); raw != "" {
			if *v, err = strconv.ParseUint(raw, 10, 64); err != nil || env == "GC_V2_SIM_SEED" && os.Getenv("GC_V2_SIM_SEEDS") != "" {
				t.Fatalf("%s=%q: want a number, and only one of GC_V2_SIM_SEED and GC_V2_SIM_SEEDS (%v)", env, raw, err)
			}
		}
	}
	if os.Getenv("GC_V2_SIM_SEED") != "" {
		n = 1
	}
	var seeds []uint64
	for i := range n {
		seeds = append(seeds, first+i)
	}
	return seeds
}

// Kills any merged path that breaks an invariant a run can observe: every
// seed runs simSteps steps, then quiesces.
func TestSimCorpus(t *testing.T) {
	for _, seed := range simSeeds(t) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			s := newSim(t, seed, simOpts{})
			s.run(simSteps)
			s.report(t)
		})
	}
}

// droppingInflight loses every settlement.
type droppingInflight struct{ *inflightMap }

func (droppingInflight) settle(settlement) {}

// simMutants are faults seeded into merged code, each with the invariant
// that must catch it. The PR whose code a mutant changes adds it (v5 §10).
var simMutants = []struct {
	name, inv string
	opts      simOpts
}{
	{"the in-flight map loses a settlement", "I8 ", simOpts{inflight: func(m *inflightMap) plannerInflight { return droppingInflight{m} }}},
	{"an effect reports a landing it never wrote", "I10 ", simOpts{registry: func(reg map[string]effectBuilder) map[string]effectBuilder {
		out := maps.Clone(reg)
		out[intentRowHeal] = func(*effectPass, intent) func(context.Context) settlement {
			return func(context.Context) settlement { return settlement{Outcome: settledLanded} }
		}
		return out
	}}},
}

// Kills invariant checks that check nothing: the corpus catches each mutant
// on its invariant, with stale events off so no other fault stands in.
func TestSimInvariantsCatchSeededMutants(t *testing.T) {
	saved := simStaleEvents
	simStaleEvents = false
	t.Cleanup(func() { simStaleEvents = saved })
	for _, m := range simMutants {
		t.Run(m.name, func(t *testing.T) {
			for seed := uint64(1); seed <= simCISeeds; seed++ {
				var s *sim
				t.Run(fmt.Sprint(seed), func(t *testing.T) {
					s = newSim(t, seed, m.opts)
					s.run(simSteps)
				})
				if slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, m.inv) }) {
					return
				}
			}
			t.Errorf("no seed of %d caught the mutant on %s", simCISeeds, m.inv)
		})
	}
}
