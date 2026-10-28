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
	"sync/atomic"
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
// effectSpecs holds runs here unchanged, files add invariants through the
// sim*Checks hooks, and scenarios drive sim's steps directly. A release may
// run an outside writer inside the effect, at one of runTx's seams, and
// every seam checks the transaction's lock scope.

const (
	simPatrol = 10 * time.Second
	simSteps  = 500
	// simMaxPacedRounds bounds I10's paced re-issues at quiescence (v5.9n).
	simMaxPacedRounds = 8
	// simQuietExtension caps how far row timers armed during quiescence
	// move I10's bound (quietBound).
	simQuietExtension = 15 * time.Minute
	simCISeeds        = 64
	// simGuard is real time: it turns a hang into a failure that names the
	// seed. Nothing the sim waits on is paced by real time (its clock is
	// fake, and a deadline cancels inside the Advance that reaches it), so
	// only a hang reaches it; it is generous because a loaded runner can
	// stall a goroutine for seconds (seeds 130 and 834 tripped 10s on RBE,
	// mc-gfdi5). The corpus target's own -test.timeout bounds the range.
	simGuard   = 2 * time.Minute
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
	restart          bool // GC_RESTART_REQUESTED, as tmux's user-option mirror carries it (C7a-2b)
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
	Effect       string // the intent kind of the v2 effect that made it, when one ran
	Reason       string // that effect's reason
}

var errSimListing = errors.New("sim: list-sessions failed")

// errSimEmptyName is the provider's refusal of an empty session name, as
// tmux refuses one.
var errSimEmptyName = errors.New("sim: empty session name")

// simProvider is a fake tmux backend over runtime.Fake, whose other knobs
// (peek, pending) it keeps: it lists corpses, keeps identity env per runtime,
// and records every Start and destructive call. Once listed it caches as tmux
// does: a plain liveness read (ObserveLivenessWithError, so
// ObserveLivenessBounded) answers from the last listing, and only
// ObserveLivenessSince reads fresh, each with LL2's Present bit and object
// ids.
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
	confirmable bool   // a down server confirms dead (LL2)
	external    bool   // the calls now are an operator's or a second controller's
	effect      string // the intent kind of the v2 effect now running (reconcile_sim_pending_test.go)
	reason      string // its reason
	nextStart   error  // the next Start's scripted outcome
	starts      []simStart
	kills       []simKill
	cached      map[string]simRuntime // the runtimes at the last listing
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

// listed caches the runtimes as a listing sees them.
func (p *simProvider) listed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = make(map[string]simRuntime, len(p.rts))
	for name, rt := range p.rts {
		p.cached[name] = *rt
	}
}

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
		at := p.Activity[name] // the embedded fake's, as tmux's batched read carries it
		out[name] = runtime.InventoryEntry{Incarnation: inc, DeadKnown: true, AllPanesDead: rt.corpse, AttachedKnown: true, Attached: rt.attached, ActivityKnown: !at.IsZero(), LastActivity: at, RestartRequested: rt.restart}
	}
	return out, nil
}

// SetMeta and RemoveMeta keep the restart marker on the runtime, which the
// marker lives and dies with, as a tmux session's environment does.
func (p *simProvider) SetMeta(name, key, value string) error {
	p.setRestart(name, key, value != "")
	return p.Fake.SetMeta(name, key, value)
}

func (p *simProvider) RemoveMeta(name, key string) error {
	err := p.Fake.RemoveMeta(name, key)
	if err == nil { // a failed removal keeps the marker
		p.setRestart(name, key, false)
	}
	return err
}

func (p *simProvider) setRestart(name, key string, set bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt := p.rts[name]; rt != nil && key == "GC_RESTART_REQUESTED" {
		rt.restart = set
	}
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

// ObserveLivenessWithError answers from the last listing, once one ran.
func (p *simProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rts[name]
	if p.cached != nil {
		rt = nil
		if c, ok := p.cached[name]; ok {
			rt = &c
		}
	}
	return livenessOf(name, rt)
}

// ObserveLivenessSince reads the runtime as it is now.
func (p *simProvider) ObserveLivenessSince(name string, _ []string, _ time.Time) (runtime.Liveness, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return livenessOf(name, p.rts[name])
}

func livenessOf(name string, rt *simRuntime) (runtime.Liveness, error) {
	if rt != nil && rt.probeErr && !rt.corpse {
		l := runtimeLiveness(rt)
		l.Alive = false
		return l, fmt.Errorf("sim: probe of %s: %w", name, runtime.ErrRuntimeUnavailable)
	}
	return runtimeLiveness(rt), nil
}

func runtimeLiveness(rt *simRuntime) runtime.Liveness {
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

// IsAttachedWithError is the attach fact a fence's L3 reads, the
// simulator's own rather than runtime.Fake's; a runtime whose probes err
// cannot answer it.
func (p *simProvider) IsAttachedWithError(name string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt := p.rts[name]; rt != nil && rt.probeErr {
		return false, fmt.Errorf("sim: attach probe of %s: %w", name, runtime.ErrRuntimeUnavailable)
	}
	return p.rts[name] != nil && p.rts[name].attached, nil
}

// Start creates a runtime carrying cfg's identity env, unless a scripted
// outcome fails it or the name is held (FreshOnly's ErrSessionExists).
func (p *simProvider) Start(_ context.Context, name string, cfg runtime.Config) error {
	if name == "" {
		return errSimEmptyName // tmux refuses an empty session name
	}
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
		if _, tracked := p.Activity[name]; tracked {
			p.SetActivity(name, p.now()) // a new session's activity is its creation, as tmux's window_activity
		}
	}
	p.starts = append(p.starts, s)
	return err
}

func (p *simProvider) Stop(name string) error {
	if name == "" {
		return errSimEmptyName
	}
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
	p.kills = append(p.kills, simKill{Method: method, Name: name, Hit: hit, External: p.external, Effect: p.effect, Reason: p.reason})
}

// simBacking is one leg's revisioned store: a MemStore stamped require, whose
// reads the cache serves exactly, as the controller's SQLite binding.
type simBacking struct{ *beads.MemStore }

func (simBacking) CachedReadExact() bool { return true }

// simLegBacking is the simulator's leg backing: simBacking with the atomic
// terminal close the v2 close verbs require (closeRow), as the city's
// SQLite sessions store has.
type simLegBacking struct{ simBacking }

func (b simLegBacking) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	return beads.AtomicCloserOver(b.MemStore).CloseWithMetadataIfMatch(id, expectedRevision, metadata)
}

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
	// seam and outside are an outside writer the release runs inside the
	// effect, at that seam of runTx, on the effect's row; seams are the
	// seams it reached.
	seam    txSeam
	outside func()
	seams   []txSeam
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
	notIdle  []string     // released idle probes that found the agent busy, as intentKeys
	parked   []*simEffect
	lastSeq  uint64
	prev     map[string]beads.Bead // "leg/id" → row, after the last step
	// respawnBegun are the (row, bead) pairs with an idle-respawn begin
	// (I25).
	respawnBegun map[string]bool
	// noPendingCreates is simOpts.noPendingCreates (reconcile_sim_pending_test.go).
	noPendingCreates bool
	// stopAdmissions are the stops each pass admitted, by row
	// (reconcile_sim_pending_test.go), for I16's moved-basis excuse.
	stopAdmissions map[rowKey]stopAdmission
	// killsChecked is how many of the provider's kills I4 and I5 judged.
	killsChecked int
	// closedInStep is, by session name, each row an outside writer closed
	// since the last v2 audit, as it stood before the close (I4, v5.9n).
	closedInStep map[string]beads.Bead
	// preOutside is the store an outside write found, while it runs, by
	// which closedInStep holds a row as its close found it.
	preOutside map[string]beads.Bead
	// basisMoves are the outside writes that moved a row's drain basis, as
	// "leg/id", in order (I16, v5.9n); writes is cleared every step.
	basisMoves []string
	// lastWriter is, by "leg/id" and key, the actor whose write last changed
	// the key; premiseRefusals are the rows, as "leg/id", whose own effect
	// refused on its premise over a key an outside writer last changed, in
	// order (I15's #57 clock, v5.9x).
	lastWriter      map[string]map[string]string
	premiseRefusals []string
	// round is the effects quiescence's current round released, with
	// their settlements (I10's paced retries, v5.9n).
	round    []simResult
	writes   []simWrite
	admitted []time.Time // admitted starts (I9)
	// stable is, by row ID, whether the pass that last admitted an effect
	// for the row read it committed, its own runtime alive, and 30s past its
	// wake: I15's view of A6's stability clear.
	stable map[string]bool
	// preWakeQuarantine is each row's quarantined_until at v2's last PreWake
	// of it; quarantinedCommits the rows whose commit kept a quarantine that
	// landed after it (§12.2 #57), by their bound's clock (v5.9x).
	preWakeQuarantine  map[string]string
	quarantinedCommits map[string]*quarantinedClock
	failures           []string
	locks              *lockObserver // the sections holding each row's mutation lock
	runMu              sync.Mutex
	running            map[effectKey]*simEffect // released, by key, for their seams
	credit             decisionCredit           // the arm decisions the census credits (reconcile_sim_coverage_test.go)
	envNow             *reconcileEnv            // the environment the next gather reads; a config drift replaces it
	baseCfg            *config.City             // the seed's config, which a drift's revert restores
}

// simOpts scripts a run: rows replaces the seeded rows, and the other fields
// seed one fault into the code under test through a test seam.
type simOpts struct {
	rows     func(s *sim) (city, rig []beads.Bead)
	inflight func(*inflightMap) plannerInflight
	specs    func(map[string]effectSpec)
	// rowLock replaces runTx's session mutation lock, unobserved.
	rowLock func(id string, fn func() error) error
	arms    func([]rowArm) []rowArm
	// noPendingCreates seeds no pending creates (reconcile_sim_pending_test.go):
	// the floored corpus keeps its floor members' acks in reach.
	noPendingCreates bool
}

func newSim(t *testing.T, seed uint64, o simOpts) *sim {
	s := &sim{
		t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0x51d1a)), clk: newFakePlannerClock(plannerT0), obsClk: &clock.Fake{Time: plannerT0},
		parkCh: make(chan *simEffect, 256), postCh: make(chan settlement, 256), stable: map[string]bool{},
		running: make(map[effectKey]*simEffect),
	}
	s.lag = s.rng.IntN(2) == 0
	s.sp = &simProvider{Fake: runtime.NewFake(), rts: make(map[string]*simRuntime), changed: make(map[string]uint64), now: s.clk.Now}
	s.cfg = workerCity(3)
	s.cfg.Daemon.PatrolInterval, s.cfg.Daemon.ProbeConcurrency = simPatrol.String(), intPtr(1)
	s.cfg.Rigs = []config.Rig{{Name: simRigLeg, Path: t.TempDir()}}

	if o.rows == nil {
		o.rows = (*sim).seedRows
	}
	s.noPendingCreates = o.noPendingCreates
	city, rig := o.rows(s)
	for name, rows := range map[string][]beads.Bead{simCityLeg: city, simRigLeg: rig} {
		m := beads.NewMemStoreFrom(0, rows, nil)
		if err := beads.StampOpenedStore(m, "MemStore", gate.Require, false, nil, nil); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		cache := beads.NewCachingStoreForTest(simLegBacking{simBacking{m}}, nil, beads.WithClock(s.clk.Now)) // its windows run on sim time
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("prime %s: %v", name, err)
		}
		if got := cache.Stats().LastFreshAt; !got.Equal(s.clk.Now()) {
			t.Fatalf("%s cache: LastFreshAt %v, want the sim clock %v (its windows must run on sim time)", name, got, s.clk.Now())
		}
		s.legs = append(s.legs, &simLeg{name: name, backing: m, cache: cache})
	}
	slices.SortFunc(s.legs, func(a, b *simLeg) int { return strings.Compare(a.name, b.name) }) // city, then rig-a

	s.lane = newRuntimeInventoryLane(simPatrol, io.Discard, "sim")
	s.lane.clock, s.lane.cache = s.obsClk, NewObservationCache(s.obsClk, 2*simPatrol, "sim")
	s.envNow = &reconcileEnv{Gen: 1, Cfg: s.cfg, SP: s.sp}
	s.env = gatherEnv{
		CityPath: t.TempDir(), CityName: "test-city",
		Env:          func() *reconcileEnv { return s.envNow }, // a config drift swaps it (reconcile_sim_drift_test.go)
		Sessions:     func() beads.Store { return s.legs[0].cache },
		WorkStore:    func() beads.Store { return s.legs[0].cache }, // the city's work and sessions share a store
		RigStores:    func() map[string]beads.Store { return map[string]beads.Store{simRigLeg: s.legs[1].cache} },
		Recording:    func() *externalReadsRecording { return nil },
		Observations: func() *ObservationCache { return s.lane.cache },
		Episodes: func() (map[string]session.StartupHealthEpisode, error) {
			return readStartupHealthEpisodes(s.legs[0].cache)
		},
		// The production resolver over the sessions leg, so a v2 start
		// launches the template legacy's start launches.
		Templates: func(env *reconcileEnv, now time.Time) templateResolver {
			return newTemplateResolver(newAgentBuildParams("test-city", s.env.CityPath, env.Cfg, env.SP, now, s.legs[0].cache, io.Discard))
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
	s.x = newEffectExecutor(func(st settlement) {
		countPost(st) // reconcile_sim_coverage_test.go: the census counts what the planner is told
		s.p.settlements.post(st)
		s.postCh <- st
	}, io.Discard)
	s.x.clock = s.clk
	s.x.spawn = func(f func()) {
		s.spawned.Add(1)
		go func() {
			defer s.spawned.Done()
			f()
		}()
	}
	s.p = newPlanner(s.clk, func() time.Duration { return simPatrol }, nil, inflight, nil, io.Discard)
	s.p.pass = func(now time.Time) (res passResult) {
		s.credit.planning(func() { res = s.p.tracePass(s.env, now) }) // the census credits its decisions
		return res
	}
	s.p.effects = s.x
	// The create effects (C1, C2), on the sessions leg under the city's
	// identifier locks, so the allocation's plans land rows (mc-i7efm).
	creates, err := newCreateEffects(createEffectHost{cityPath: s.env.CityPath, cityName: s.env.CityName, lookPath: s.env.LookPath, now: s.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	s.p.creates = creates
	// The planner notes effects' fresh observations in the lane's cache, as
	// production wires it (plannerRuntime.bindHost): a start's alive read
	// and any effect's absent one (CONTRACT O4, v5.10g).
	s.p.observations = func() *ObservationCache { return s.lane.cache }
	if simPristineSpecs == nil {
		simPristineSpecs = effectSpecs
		t.Cleanup(func() { simPristineSpecs = nil })
	}
	saved := effectSpecs
	effectSpecs = maps.Clone(simPristineSpecs)
	t.Cleanup(func() { effectSpecs = saved })
	if o.specs != nil {
		o.specs(effectSpecs)
	}
	s.gate(effectSpecs)
	s.p.seam = s.atSeam
	s.locks = observeRowLocks(t)
	if o.rowLock != nil {
		saved := withRowMutationLock
		withRowMutationLock = o.rowLock
		t.Cleanup(func() { withRowMutationLock = saved })
	}
	savedArms, arms := rowArms, slices.Clone(rowArms)
	if o.arms != nil {
		arms = o.arms(arms)
	}
	rowArms = countArms(arms, &s.credit) // reconcile_sim_coverage_test.go
	t.Cleanup(func() { rowArms = savedArms })
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
		// Woken up to 10m ago: some inside INC-003's 5m grace, some past it,
		// so the corpus admits A20's begins (C6a2 review).
		meta := []string{"generation", strconv.Itoa(gen), "instance_token", token, "last_woke_at", s.rel(-time.Duration(s.rng.IntN(600)) * time.Second)}
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
	if !s.noPendingCreates {
		city = append(city, s.pendingCreateRows()...) // reconcile_sim_pending_test.go
	}
	for i := 1; i <= 1+s.rng.IntN(2); i++ {
		rig = append(rig, row(fmt.Sprintf("rg-%d", i), 10+i))
	}
	return city, rig
}

func (s *sim) rel(d time.Duration) string { return s.clk.Now().Add(d).UTC().Format(time.RFC3339) }

// gate wraps every effect in specs (around, outside the spec's own) so it
// parks until a step releases it.
// simPristineSpecs are the specs before any sim in the test gated them: a
// second sim gates these, never the first one's gated effects, whose
// effects would park on the first sim's channel.
var simPristineSpecs map[string]effectSpec

func (s *sim) gate(specs map[string]effectSpec) {
	for kind, spec := range specs {
		if !spec.runs() {
			continue
		}
		inner := spec.around
		spec.around = func(ctx context.Context, c aroundCaps, run func() settlement) settlement {
			e := &simEffect{it: c.it, release: make(chan struct{}), done: make(chan settlement, 1)}
			s.parkCh <- e
			<-e.release
			key := effectKey{row: c.it.Key, token: c.it.CreatePlan.Token}
			s.runMu.Lock()
			s.running[key] = e
			s.runMu.Unlock()
			res := run
			if inner != nil {
				res = func() settlement { return inner(ctx, c, run) }
			}
			out := res()
			s.runMu.Lock()
			delete(s.running, key)
			s.runMu.Unlock()
			e.done <- out
			return out
		}
		specs[kind] = spec
	}
}

// atSeam runs on an effect's goroutine at runTx's seam at, the sim's
// goroutine waiting on it: it checks the transaction's lock scope, then runs
// the effect's outside writer if this is its seam. Inside a section the
// row's session mutation lock is held, so no in-process writer lands
// between the fresh read and the CAS; after a Call it is free while the
// name lock holds, so a reaper skips the name. A spec that needs the name
// lock holds it at every seam.
func (s *sim) atSeam(_ context.Context, at txSeam, it intent, _, _ int) error {
	s.runMu.Lock()
	e := s.running[effectKey{row: it.Key, token: it.CreatePlan.Token}]
	s.runMu.Unlock()
	if e == nil {
		return nil
	}
	e.seams = append(e.seams, at)
	if k := it.Key; k.ID != "" {
		needs := effectSpecs[it.Kind].needs // a kind's per-intent needs only add to these
		row := s.prev[s.legOf(k.Leg).name+"/"+k.ID]
		switch named := nameHeld(s.t, s.env.CityPath, strings.TrimSpace(sessionInfoFromBead(row).SessionName)); {
		case s.locks.holds(k.ID) != (at != seamBeforeCall && at != seamAfterCall):
			s.failf("LOCK tx-scope", "%s effect on %s at seam %d: row mutation lock held %t", it.Kind, k.ID, at, s.locks.holds(k.ID))
		case (needs.locksName(effectSpecs[it.Kind]) || at == seamBeforeCall || at == seamAfterCall) && !named:
			s.failf("LOCK tx-scope", "%s effect on %s at seam %d: runtime name not locked", it.Kind, k.ID, at)
		}
	}
	if e.outside != nil && e.seam == at {
		op := e.outside
		e.outside = nil
		op()
	}
	return nil
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
	s.noteStopAdmissions(rec) // reconcile_sim_pending_test.go: as the pass admits them
	census, obs := s.observed()
	for _, d := range rec.Deferred {
		if strings.HasPrefix(d.Cause, "rests-") {
			s.t.Fatalf("seed %d: %s:%s deferred for its rests (%s): a fresh kind's arm must record what it rests on (ruling (c))", s.seed, d.Kind, d.Key.ID, d.Cause)
		}
	}
	for _, it := range rec.Admitted {
		if it.Kind == intentStart {
			s.admitted = append(s.admitted, now)
		}
		if it.Kind == intentDrainBegin || it.Kind == intentDrainBeginFresh {
			simDrainBegins.Add(1)
		}
		countReason(it) // reconcile_sim_coverage_test.go: the floor-ack cancel the gate requires
		if it.Reason == decideSquatterHeal {
			simSquatterHeals.Add(1)
		}
		if census != nil {
			info := census.Rows[it.Key].Info
			woke, err := time.Parse(time.RFC3339, info.LastWokeAt)
			s.stable[it.Key.ID] = committed(info) && obs[it.Key].Liveness == livenessAlive && err == nil && now.Sub(woke) >= stabilityThreshold
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
// when its deadline already settled it, for its late post.
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
	s.round = append(s.round, simResult{it: e.it, res: res})
	if res.Outcome == settledNoop && res.Cause == causeNotIdle {
		s.notIdle = append(s.notIdle, intentKeys([]intent{e.it})...)
	}
	for _, c := range after {
		c(res)
	}
	if !e.settled || !res.Facts.empty() { // a late return posts only its facts
		s.awaitPost(func(st settlement) bool {
			return st.Key == e.it.Key && st.Kind == e.it.Kind && st.Token == e.it.CreatePlan.Token && st.Late == e.settled
		})
	}
}

// releaseInside releases parked effect i with an outside writer, picked at
// random, run on its row at a random seam inside its section; for a start,
// also after its launch Call, between the Start and the commit (S2's lost
// commit).
func (s *sim) releaseInside(i int) {
	e := s.parked[i]
	seams := []txSeam{seamAfterRowRead, seamBeforeCAS}
	if e.it.Kind == intentStart {
		seams = append(seams, seamAfterCall)
	}
	e.seam = seams[s.rng.IntN(len(seams))]
	op := s.pickExternalOp()
	fire := func() {
		l := s.legOf(e.it.Key.Leg)
		if b, err := l.backing.Get(e.it.Key.ID); err == nil {
			s.outside(op.name+" inside "+e.it.Kind, l, b, func(l *simLeg, b beads.Bead) { op.fn(s, l, b) })
		}
	}
	e.outside = fire
	if e.seam == seamAfterCall { // the next Call's seam (the admit ticket's, or a recycle's kill) comes first: re-arm for the one after it
		e.outside = func() { e.seam, e.outside = seamAfterCall, fire }
	}
	s.release(i)
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
	s.sp.listed()
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
	s.outside(name, l, rows[s.rng.IntN(len(rows))], fn)
}

// outside runs fn on row b of leg l as an outside writer, and audits it.
func (s *sim) outside(name string, l *simLeg, b beads.Bead, fn func(l *simLeg, b beads.Bead)) {
	s.preOutside = s.snapshot() // the store as the outside write found it (v5.9n amended)
	defer func() { s.preOutside = nil }()
	s.sp.external = true // no effect calls the provider while it runs
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
	{"operator-hold", func(s *sim, l *simLeg, b beads.Bead) { // gc session suspend under a controller: the hold, on a live runtime
		_, _ = session.NewStore(beads.SessionStore{Store: l.backing}).OperatorSuspend(b.ID, s.clk.Now())
	}},
	{"operator-resume", func(s *sim, l *simLeg, b beads.Bead) { // gc session resume: the operator's hold consumed
		_, _ = session.NewStore(beads.SessionStore{Store: l.backing}).ApplyDispositionFresh(b.ID, session.ActorOperator, session.DispEvent{Kind: session.EvResume}, nil, s.clk.Now())
	}},
	{"heartbeat", func(s *sim, l *simLeg, b beads.Bead) { // gc runtime heartbeat: held_until alone, an operator's suspend left as it is
		until := s.clk.Now().Add(time.Duration(1+s.rng.IntN(120)) * time.Second)
		_, _ = session.NewStore(beads.SessionStore{Store: l.backing}).ApplyDispositionFresh(b.ID, session.ActorAgent, session.DispEvent{Kind: session.EvHeartbeat, Until: until}, nil, s.clk.Now())
	}},
	{"quarantine", func(s *sim, l *simLeg, b beads.Bead) {
		s.setMeta(l, b.ID, "quarantined_until", s.rel(time.Duration(1+s.rng.IntN(120))*time.Second), "sleep_reason", "quarantine")
	}},
	{"kill", func(s *sim, l *simLeg, b beads.Bead) { // gc session kill: the fence, then the stop
		if _, err := session.WriteKillFence(l.backing, b.ID, s.clk.Now()); err != nil && !errors.Is(err, session.ErrKillFenceSuperseded) {
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
	{"suspend", func(s *sim, l *simLeg, b beads.Bead) { // the stop and rig-restart sweeps' soft suspend (v5.9u): the stop, then the state
		_ = s.sp.Stop(b.Metadata["session_name"])
		s.setMeta(l, b.ID, "state", string(session.StateSuspended), "suspended_at", s.rel(0), "slept_at", "", "sleep_reason", "")
	}},
}

// ensureRunningAllows is Manager.ensureRunning's refusal before any start: a
// kill-fenced row, or one whose drain-ack stop is pending (the controller's
// stop owns its runtime), starts nothing.
func (s *sim) ensureRunningAllows(b beads.Bead) bool {
	m := b.Metadata
	return !session.KillPendingMetadata(m["state"], m["state_reason"], m["sleep_reason"], m["slept_at"], s.clk.Now()) &&
		(m["state"] != string(session.StateDraining) || m["state_reason"] != session.DrainAckStopPendingReason)
}

// ensureRunningConfirms is Manager.ensureRunning's write once the runtime
// is up, started by this attach or found running (started false):
//   - on a row an operator holds (KeepsSlot) whose runtime it started, or
//     that is suspended, active or awake (liveUserHoldDecision for a running
//     one), the hold's consume (consumeUserHold): the operator's Resume
//     cell, and the row active with its wake request, sleep reason, slept_at
//     and claim cleared; nothing at all when the cell refuses (a draining
//     row);
//   - otherwise confirmLiveSessionState: the row active from a dormant or
//     uncommitted state, its claim cleared.
//
// Either clears the stop request, operator_drain included
// (restartStopRequestClears), only for a runtime it started, as PreWake
// does (#7635), so the controller never adopts an operator's attach
// (mc-946b6 seed 848's sim gap; the #7765 review).
func (s *sim) ensureRunningConfirms(l *simLeg, b beads.Bead, started bool) {
	m, now := b.Metadata, s.clk.Now()
	patch := session.MetadataPatch{}
	if started {
		for k := range session.ClearStopRequestPatch() {
			if m[k] != "" {
				patch[k] = ""
			}
		}
		if m[session.OperatorDrainKey] != "" {
			patch[session.OperatorDrainKey] = ""
		}
	}
	state := session.State(strings.TrimSpace(m["state"]))
	d := session.DecodeDisposition(m)
	// ensureRunning consumes the hold where it starts the runtime (a dead
	// one, or a suspended row's), or over a running one only on an active or
	// awake row (liveUserHoldDecision); every other held row it confirms.
	if consumes := started || state == session.StateSuspended || state == session.StateActive || state == session.StateAwake; d.KeepsSlot() && consumes {
		to, err := d.Apply(session.ActorOperator, session.DispEvent{Kind: session.EvResume}, state, now)
		if err != nil {
			return // the Resume cell refused (a draining row): consumeUserHold writes nothing
		}
		maps.Copy(patch, to.Changed().Fold())
		maps.Copy(patch, session.ClearWakeRequestPatch())
		for k, v := range map[string]string{"state": string(session.StateActive), "state_reason": "creation_complete", "sleep_reason": "", "slept_at": "", "pending_create_claim": "", "pending_create_started_at": ""} {
			patch[k] = v
		}
	} else {
		switch state {
		case "", session.StateStartPending, session.StateCreating, session.StateAsleep, session.StateSuspended:
			patch["state"], patch["state_reason"] = string(session.StateActive), "creation_complete"
		}
		if strings.TrimSpace(m["pending_create_claim"]) != "" {
			patch["pending_create_claim"], patch["pending_create_started_at"] = "", ""
		}
	}
	var kv []string
	for _, k := range slices.Sorted(maps.Keys(patch)) {
		if m[k] != patch[k] {
			kv = append(kv, k, patch[k])
		}
	}
	if len(kv) > 0 {
		s.setMeta(l, b.ID, kv...)
	}
}

// runtimeOpStarts are the outside starts among runtimeOps, by name, with
// the row write each makes once its runtime is up; each runs only when
// ensureRunning would start at all.
var runtimeOpStarts = map[string]func(s *sim, l *simLeg, b beads.Bead, started bool){
	"attach-recreates": (*sim).ensureRunningConfirms, // gc session attach
}

// runtimeOp runs the runtime op name (fn) on row b. An outside start
// (runtimeOpStarts) runs only where ensureRunning would start at all
// (ensureRunningAllows), starts nothing over a running session, and then
// writes the row as ensureRunning does.
func (s *sim) runtimeOp(name string, fn func(p *simProvider, name string, b beads.Bead), l *simLeg, b beads.Bead) {
	confirm := runtimeOpStarts[name]
	if confirm != nil && !s.ensureRunningAllows(b) {
		return
	}
	sessName := b.Metadata["session_name"]
	s.sp.mu.Lock()
	rt := s.sp.rts[sessName]
	running := rt != nil && !rt.corpse
	if confirm == nil || !running {
		fn(s.sp, sessName, b)
	}
	s.sp.mu.Unlock()
	if confirm != nil {
		confirm(s, l, b, !running)
	}
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
	s.beginStep()
	switch r := s.rng.IntN(100); {
	case r < 22:
		s.pass()
	case r < 36:
		switch i := s.rng.IntN(max(len(s.parked), 1)); {
		case len(s.parked) == 0:
		case s.parked[i].it.Key.ID != "" && s.rng.IntN(3) == 0:
			s.releaseInside(i)
		default:
			s.release(i)
		}
	case r < 46:
		s.advance(time.Duration(1+s.rng.IntN(int(simPatrol/time.Second))) * time.Second)
	case r < 56:
		s.inventory()
	case r < 70:
		s.deliver()
	case r < 84:
		op := s.pickExternalOp()
		s.external(op.name, func(l *simLeg, b beads.Bead) { op.fn(s, l, b) })
		return
	default:
		op := s.pickRuntimeOp()
		s.external("runtime "+op.name, func(l *simLeg, b beads.Bead) { s.runtimeOp(op.name, op.fn, l, b) })
		return
	}
	s.audit("v2")
}

// beginStep starts a step: an outside close of an earlier step excuses no
// kill in this one (I4's rowNamed, v5.9n).
func (s *sim) beginStep() { clear(s.closedInStep) }

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
		s.noteWriter(k, actor, before, after)
		if actor == "v2" {
			continue
		}
		if before.ID != "" && len(session.FactsDrainBasis.Moved(session.FactsOf(after.Metadata), session.FactsOf(before.Metadata))) > 0 {
			s.basisMoves = append(s.basisMoves, k)
		}
		if before.ID != "" && before.Status != "closed" && after.Status == "closed" {
			if s.closedInStep == nil {
				s.closedInStep = map[string]beads.Bead{}
			}
			// The closed row as the store held it immediately before its
			// closing write, the step's own earlier writes (a PreWake's
			// token mint) included, not the step-start snapshot (v5.9n
			// amended).
			held := before
			if p, ok := s.preOutside[k]; ok && p.Status != "closed" {
				held = p
			}
			s.closedInStep[held.Metadata["session_name"]] = held
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

// run runs simSteps steps until a violation, then quiesces.
func (s *sim) run() {
	s.inventory()
	for range simSteps {
		if s.step(); len(s.failures) > 0 {
			return
		}
	}
	s.quiesce()
}

// runShifted is run with one extra RNG draw before every step: the same
// operations on a schedule the corpus never takes, so a model gap that fits
// only main's stream shows here (mc-946b6).
func (s *sim) runShifted() {
	s.inventory()
	for range simSteps {
		_ = s.rng.IntN(100)
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
	s.x.cancel(errEffectShutdown)
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

// report fails t with the seed's violations, the command that reruns t's
// top-level test on the seed, and its schedule.
func (s *sim) report(t *testing.T) {
	t.Helper()
	if len(s.failures) > 0 {
		test, _, _ := strings.Cut(t.Name(), "/")
		t.Errorf("seed %d (inventory lag %t) violated:\n  %s\nreplay: bazel test //cmd/gc:gc_test --test_sharding_strategy=disabled --test_filter='^%s$' --test_env=GC_V2_SIM_SEED=%d\nschedule:\n%s",
			s.seed, s.lag, strings.Join(s.failures, "\n  "), test, s.seed, strings.Join(s.schedule, "\n"))
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
// until 2 × patrol + startup_timeout past the last pending timer, as read
// after the first such pass (which records the settlements still pending).
// From
// there, two passes in a row must admit nothing and change nothing (I10),
// and the in-flight map must then be empty (I8).
func (s *sim) quiesce() {
	s.beginStep() // quiescence is a step of its own: an earlier close excuses no kill in it
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
	ceiling := bound.Add(simQuietExtension)
	rounds := quiesceRounds{}
	for first, done := true, false; !done && len(s.failures) == 0; first = false {
		before, backoff, at := s.snapshot(), s.p.backoff.Snapshot(), s.clk.Now()
		s.round = nil
		s.inventory()
		s.pass()
		admitted := intentKeys(s.p.out.record.Load().Admitted)
		for len(s.parked) > 0 {
			s.release(0)
		}
		s.audit("v2")
		if b := s.lastTimer().Add(slack); first && b.After(bound) {
			// Once, after the first pass: it records the settlements still
			// pending at quiescence, so the backoff of the last refusal
			// before it is in the table, and the bound covers it.
			bound = b
		}
		bound = quietBound(bound, s.rowTimers(), s.clk.Now(), slack, ceiling)
		after := s.snapshot()
		changed := !maps.EqualFunc(before, after, sameBead)
		switch {
		case s.clk.Now().Before(bound):
		case (len(admitted) > 0 || changed) && s.attachedDriftRestamps(before, after):
			// A14's throttled attached-deferral re-stamp on a row that stays
			// attached and drifted (CONTRACT v5.10m): no round of progress,
			// nor a failure.
		case (len(admitted) > 0 || changed) && s.pacedRetries(backoff, at, before, after):
			var keys []rowKey
			for _, r := range s.round {
				keys = append(keys, r.it.Key)
			}
			if k, loop := rounds.paced(keys); loop {
				s.failf("I10 I-bounded", "%d paced re-issues past the bound for %v: admitted %v, a retry loop", rounds.count[k], k, admitted)
			}
		case len(admitted) > 0 || changed:
			s.failf("I10 I-bounded", "no fixed point %s after the last timer: admitted %v, store changed %t", s.clk.Now().Sub(bound)+slack, admitted, changed)
		default:
			done = rounds.quiet()
		}
		s.advance(simPatrol)
		for _, l := range s.legs {
			// The periodic reconcile. The first rescan above ran inside a
			// local write's 5s window, which skips the row; one past it
			// re-reads the row.
			l.cache.ReconcileNowForTest()
		}
	}
	if len(s.failures) == 0 {
		s.checkConverged()
	}
	for _, c := range simQuietChecks {
		c(s)
	}
	if n := len(s.inflight.view().Entries); n > 0 && len(s.failures) == 0 {
		s.failf("I8 I-inflight", "%d entries in flight at quiescence", n)
	}
}

// checkConverged fails I10 when a leg's cache is clean (no dirty row, so it
// answers CachedList) but disagrees with its backing: a stale row that no
// event or scan will revisit, so the fixed point the pass saw was the cache's.
func (s *sim) checkConverged() {
	for _, l := range s.legs {
		cached, ok := l.cache.CachedList(beads.ListQuery{AllowScan: true})
		if !ok {
			continue
		}
		active, err := l.backing.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			s.t.Fatal(err)
		}
		want := make(map[string]beads.Bead, len(active))
		for _, b := range active {
			want[b.ID] = b
		}
		for _, b := range cached {
			if w, ok := want[b.ID]; !ok || !sameBead(b, w) {
				s.failf("I10 I-bounded", "cache not converged at quiescence: %s/%s cached %s %v, backing %s %v", l.name, b.ID, b.Status, b.Metadata, w.Status, w.Metadata)
			}
			delete(want, b.ID)
		}
		for id := range want {
			s.failf("I10 I-bounded", "cache not converged at quiescence: %s/%s active in the backing, missing from the clean cache", l.name, id)
		}
	}
}

// quiesceRounds counts I10's rounds past the bound (v5.9n, amended): two
// quiet rounds in a row end quiescence; a paced re-issue round is neither
// quiet nor a failure, and resets the quiet run; more than
// simMaxPacedRounds paced re-issues of one row is a retry loop (a backoff
// that never grows), counted per row, so correctly paced rows never add up.
type quiesceRounds struct {
	quiets int
	count  map[rowKey]int
}

// quiet records a quiet round and reports whether quiescence is reached.
func (q *quiesceRounds) quiet() bool {
	q.quiets++
	return q.quiets >= 2
}

// paced records a round of paced re-issues of keys, and returns a row whose
// count passed the cap.
func (q *quiesceRounds) paced(keys []rowKey) (rowKey, bool) {
	q.quiets = 0
	if q.count == nil {
		q.count = map[rowKey]int{}
	}
	for _, k := range keys {
		if q.count[k]++; q.count[k] > simMaxPacedRounds {
			return k, true
		}
	}
	return rowKey{}, false
}

// simResult is a released effect and its settlement.
type simResult struct {
	it  intent
	res settlement
}

// pacedRetries is I10's paced retry (v5.9n): a quiescence round whose
// effects all refused a re-issue the contract holds, each no earlier than
// its row's backoff expiry (a paced retry, not a loop), and whose only store
// change is the runtime lease record each attempt writes. The holds: a stop
// a human's attach holds (§12.2 #10), and a stop, rollback or fresh heal
// refused liveness-unknown while its runtime's probe keeps erroring (I17,
// v5.10l).
func (s *sim) pacedRetries(backoff map[string]backoffRecord, at time.Time, before, after map[string]beads.Bead) bool {
	for k, b := range after {
		a, ok := before[k]
		if !ok || a.Status != b.Status || !maps.EqualFunc(withoutLeaseKeys(a.Metadata), withoutLeaseKeys(b.Metadata), func(x, y string) bool { return x == y }) {
			return false
		}
	}
	if len(before) != len(after) || len(s.round) == 0 {
		return false
	}
	for _, r := range s.round {
		var row beads.Bead // the sim's row IDs are unique across its legs
		for _, b := range after {
			if b.ID == r.it.Key.ID {
				row = b
			}
		}
		held := r.it.Kind == intentStop && r.res.Cause == fenceAttached ||
			// A probe that keeps erroring holds the stop, as I17 rules it
			// not stoppable (v5.9n).
			r.it.Kind == intentStop && r.res.Cause == fenceLivenessUnknown && s.probeErring(row) ||
			// Likewise a rollback (A10): a runtime whose probe keeps erroring
			// leaves its fresh read unknown, and C3 refuses on it
			// (mc-cjtkj; seed 290).
			r.it.Kind == intentRollback && r.res.Cause == causeLivenessUnknown && s.probeErring(row) ||
			// Likewise a fresh heal (A6): the squatter heal over a Foreign
			// occupant whose probe keeps erroring cannot re-prove it, and
			// refuses (v5.10l; seeds 621, 780).
			r.it.Kind == intentRowHealFresh && r.res.Cause == causeLivenessUnknown && s.probeErring(row) ||
			// A start AL1 proposes for a row an operator holds (a pending
			// create's claim, a manual session) that its PreWake refuses
			// (RefusesPreWake), or whose recycle an attached pane refuses,
			// paced by its backoff (mc-cjtkj).
			r.it.Kind == intentStart && r.res.Cause == causeHeld && simHeld(row, at) ||
			// A start whose recycle an attached dead pane refuses (C8.1:
			// it waits for the detach), only while the sim's pane is
			// attached (seed 474 of the shifted corpus).
			r.it.Kind == intentStart && r.res.Cause == causeAttached && s.paneAttached(row)
		rec, backedOff := backoff[rowBackoffKey(r.it.Key)]
		if r.res.Outcome != settledRefused || !held || !backedOff || rec.Until.After(at) {
			return false
		}
	}
	return true
}

// attachedDriftRestamps reports a round whose only effects are A14's
// attached-deferral re-stamps (decideDriftAttached), landed, each moving only
// the stamp's two keys on a row that stays attached under the same drift,
// at most once per sessionAttachedConfigDriftRefreshInterval: legacy's
// throttled re-stamp, a periodic write by design (CONTRACT v5.10m).
func (s *sim) attachedDriftRestamps(before, after map[string]beads.Bead) bool {
	if len(s.round) == 0 || len(before) != len(after) {
		return false
	}
	healed := map[string]bool{} // the rows the round's heals re-stamped, by ID
	for _, r := range s.round {
		if r.it.Kind != intentRowHeal || r.it.Reason != decideDriftAttached || r.res.Outcome != settledLanded {
			return false
		}
		healed[r.it.Key.ID] = false
	}
	stamp := map[string]bool{sessionAttachedConfigDriftDeferredAtMetadata: true, sessionAttachedConfigDriftDeferredKeyMetadata: true}
	for k, b := range after {
		a, ok := before[k]
		switch {
		case !ok || a.Status != b.Status:
			return false
		case sameBead(a, b):
			continue
		}
		for key := range keyUnion(a.Metadata, b.Metadata) {
			if a.Metadata[key] != b.Metadata[key] && !stamp[key] {
				return false
			}
		}
		prev, err1 := time.Parse(time.RFC3339, a.Metadata[sessionAttachedConfigDriftDeferredAtMetadata])
		next, err2 := time.Parse(time.RFC3339, b.Metadata[sessionAttachedConfigDriftDeferredAtMetadata])
		key := b.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata]
		attached := s.sp.IsAttached(b.Metadata["session_name"])
		if _, ok := healed[b.ID]; !ok || err1 != nil || err2 != nil || next.Sub(prev) < sessionAttachedConfigDriftRefreshInterval || key == "" || key != a.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] || !attached {
			return false
		}
		healed[b.ID] = true
	}
	// Every heal re-stamped its row: a landed heal that moved nothing is a
	// busy loop, no throttled re-stamp (the #7859 review).
	for _, moved := range healed {
		if !moved {
			return false
		}
	}
	return true
}

// keyUnion is the keys of a and b.
func keyUnion(a, b map[string]string) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// withoutLeaseKeys is meta without the runtime lease record's keys.
func withoutLeaseKeys(meta map[string]string) map[string]string {
	out := maps.Clone(meta)
	for k := range out {
		if strings.HasPrefix(k, "runtime_lease_") {
			delete(out, k)
		}
	}
	return out
}

// quietBound is I10's bound past a row timer armed during quiescence (a
// start's churn clear, a heal's quarantine): one still ahead of now moves
// the bound out to it plus slack, never past ceiling (simQuietExtension past
// the bound read at quiescence), and never earlier than it was. The
// backoff table does not move it: its paced re-issues re-arm it.
func quietBound(bound, timer, now time.Time, slack time.Duration, ceiling time.Time) time.Time {
	if !timer.After(now) {
		return bound
	}
	b := timer.Add(slack)
	if b.After(ceiling) {
		b = ceiling
	}
	if b.After(bound) {
		return b
	}
	return bound
}

// lastTimer is the latest pending timer: a row's hold, quarantine or kill
// fence, A6's churn clear past an open committed row's wake (SESS-540), or
// a backoff's expiry.
func (s *sim) lastTimer() time.Time {
	last := s.rowTimers()
	for _, r := range s.p.backoff.Snapshot() {
		if r.Until.After(last) {
			last = r.Until
		}
	}
	// An ambiguous create holds its identity's plan until its row shows or
	// the hard bound passes (P5; ruling on seed 60 of the corpus, mc-i7efm).
	for _, e := range s.inflight.creates {
		if at := e.SettledAt.Add(inflightHardBound); e.Ambiguous && at.After(last) {
			last = at
		}
	}
	return last
}

// rowTimers is the latest timer the rows hold: lastTimer without the
// backoff table, whose paced retries re-arm it.
func (s *sim) rowTimers() time.Time {
	last := s.clk.Now()
	later := func(t time.Time) {
		if t.After(last) {
			last = t
		}
	}
	for _, b := range s.snapshot() {
		for k, grace := range map[string]time.Duration{"held_until": 0, "quarantined_until": 0, "slept_at": session.KillPendingGrace} {
			if k == "held_until" && b.Metadata["sleep_intent"] == string(session.SleepReasonUserHold) {
				continue // an operator's hold, which no timer ends
			}
			if at, err := time.Parse(time.RFC3339, b.Metadata[k]); err == nil {
				later(at.Add(grace))
			}
		}
		// A fresh pool create keeps its scaled slot postCreateProtectionTimeout
		// past its commit (poolSessionWithinPostCreateProtection), and the
		// slot's ranking moves when it ends (ruling on seed 716 of the
		// floored corpus, mc-i7efm).
		if at, err := time.Parse(time.RFC3339, b.Metadata["creation_complete_at"]); err == nil && b.Status != "closed" &&
			slices.Contains([]string{"active", "awake"}, b.Metadata["state"]) && b.Metadata["state_reason"] == "creation_complete" {
			later(at.Add(postCreateProtectionTimeout))
		}
		// The churn clear is A6's at its threshold on an open committed row
		// (SESS-540), which a patrol reaches.
		up := b.Status != "closed" && slices.Contains([]string{"active", "awake"}, b.Metadata["state"])
		if woke, err := time.Parse(time.RFC3339, b.Metadata["last_woke_at"]); err == nil && up {
			if n := b.Metadata["churn_count"]; n != "" && n != "0" {
				later(woke.Add(churnProductivityThreshold))
			}
		}
	}
	for _, t := range s.pendingCreateTimers() { // reconcile_sim_pending_test.go
		later(t)
	}
	return last
}

// simSeeds is the corpus: GC_V2_SIM_SEED replays one seed, GC_V2_SIM_SEEDS
// runs N seeds from GC_V2_SIM_SEED_FIRST (1 by default; the CI corpus
// targets run the 1024 in ranges, v5.9n), and CI's gc_test runs simCISeeds.
func simSeeds(t *testing.T) []uint64 {
	first, n := uint64(1), uint64(simCISeeds)
	for env, v := range map[string]*uint64{"GC_V2_SIM_SEED": &first, "GC_V2_SIM_SEED_FIRST": &first, "GC_V2_SIM_SEEDS": &n} {
		var err error
		if raw := os.Getenv(env); raw != "" {
			if *v, err = strconv.ParseUint(raw, 10, 64); err != nil || env == "GC_V2_SIM_SEED" && (os.Getenv("GC_V2_SIM_SEEDS") != "" || os.Getenv("GC_V2_SIM_SEED_FIRST") != "") {
				t.Fatalf("%s=%q: want a number, and GC_V2_SIM_SEED alone, without GC_V2_SIM_SEEDS or GC_V2_SIM_SEED_FIRST (%v)", env, raw, err)
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

// TestSimCorpusShiftedRNG is the corpus on the shifted stream (runShifted),
// a CI gate beside it (mc-946b6): every seed's schedule differs from main's,
// so the invariants are checked on 1024 schedules more.
func TestSimCorpusShiftedRNG(t *testing.T) {
	for _, seed := range simSeeds(t) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			s := newSim(t, seed, simOpts{})
			s.runShifted()
			s.report(t)
		})
	}
}

// Kills any merged path that breaks an invariant a run can observe: every
// seed runs simSteps steps, then quiesces.
// simDrainBegins counts the A20 begins the sims admitted.
var simDrainBegins atomic.Int64

// simSquatterHeals counts the squatter heals (CONTRACT v5.10c) the sims
// admitted.
var simSquatterHeals atomic.Int64

func TestSimCorpus(t *testing.T) {
	t.Cleanup(func() { writeSimCoverage(t) })
	seeds := simSeeds(t)
	before, squatters := simDrainBegins.Load(), simSquatterHeals.Load()
	t.Cleanup(func() {
		// The admission checks need a corpus: one replayed seed, or a
		// range smaller than CI's, need not admit either.
		if t.Failed() || os.Getenv("GC_V2_SIM_SEED") != "" || len(seeds) < simCISeeds {
			return
		}
		// Kills a corpus that never exercises A20 (C6a2 review): every
		// undesired row inside the grace, or already requested.
		if simDrainBegins.Load() == before {
			t.Error("the sim corpus admitted no A20 drain begin")
		}
		// Kills a corpus whose occupant faults never land on a committed
		// row, which would leave v5.10c's heal unchecked by every invariant.
		if simSquatterHeals.Load() == squatters {
			t.Error("the sim corpus admitted no squatter heal")
		}
	})
	for _, seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			s := newSim(t, seed, simOpts{})
			s.run()
			s.report(t)
		})
	}
}

// TestSimCorpusFloored is the corpus with a min_active_sessions floor of 1
// or 2 (by seed), so floor members' own acks are canceled (PAR-FLOORACK)
// beside every other path: no invariant may break.
func TestSimCorpusFloored(t *testing.T) {
	for _, seed := range simSeeds(t) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			s := newSim(t, seed, simOpts{noPendingCreates: true})
			s.cfg.Agents[0].MinActiveSessions = intPtr(1 + int(seed%2))
			s.run()
			s.report(t)
		})
	}
}

// TestSimCorpusPinnedSeeds (CONTRACT v5.9n). Kills a regression of the
// invariant models v5.9n fixed: the 1024-seed corpus's seeds that failed on
// main at e11c512e1c (I4: a just-closed row's own runtime; I16: an outside
// drain-basis write mid-finalize; I17: a probe-erroring runtime; I10: paced
// re-issues held by an attach) each run as a
// regression.
func TestSimCorpusPinnedSeeds(t *testing.T) {
	for _, seed := range []uint64{107, 337, 338, 427, 452, 465, 553, 657} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			s := newSim(t, seed, simOpts{noPendingCreates: true}) // main's scenarios: the seeds v5.9n pins
			s.run()
			s.report(t)
		})
	}
}

// TestSimCorpusFlooredPinnedSeeds (CONTRACT v5.10l). Kills an I10 model
// that calls a paced fresh heal a loop: in floored seeds 621 and 780 the
// squatter heal over a Foreign occupant whose probe keeps erroring refuses
// liveness-unknown on its backoff through quiescence (mc-su7hv).
func TestSimCorpusFlooredPinnedSeeds(t *testing.T) {
	for _, seed := range []uint64{621, 780} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			s := newSim(t, seed, simOpts{noPendingCreates: true}) // TestSimCorpusFloored's scenario
			s.cfg.Agents[0].MinActiveSessions = intPtr(1 + int(seed%2))
			s.run()
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
	{"an effect reports a landing it never wrote", "I10 ", simOpts{specs: func(specs map[string]effectSpec) {
		heal := specs[intentRowHeal]
		heal.sections, heal.body = nil, func(context.Context, txCaps) settlement { return settlement{Outcome: settledLanded} }
		specs[intentRowHeal] = heal
	}}},
	{"an effect section runs outside the row's mutation lock", "LOCK ", simOpts{rowLock: func(_ string, fn func() error) error { return fn() }}},
	// The PreWake keeps the row's token rather than minting one, so a row's
	// second start creates a runtime under its first one's token. (A launch
	// over a live runtime never reaches I2: S1's verb section refuses it.)
	{"a PreWake reuses the row's token", "I2 ", simOpts{
		specs: func(specs map[string]effectSpec) {
			reuse := func(ctx context.Context, r effectReads, v txView) (preWakeFold, error) {
				f, err := foldPreWake(ctx, r, v)
				f.token = v.Row.InstanceToken
				return f, err
			}
			s := specs[intentStart]
			s.sections = slices.Clone(startSections)
			s.sections[slices.IndexFunc(s.sections, func(sec section) bool { return sec.callKind == callStart })] = called(callStart, probed(reuse, preWakeStep), startLaunch)
			specs[intentStart] = s
		},
	}},
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
					s.run()
				})
				if slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, m.inv) }) {
					return
				}
			}
			t.Errorf("no seed of %d caught the mutant on %s", simCISeeds, m.inv)
		})
	}
}

// Kills I10's bound left behind a row timer armed during quiescence (shifted
// seeds 99, 668: a start's churn clear), or moved by one it should not
// follow: a timer ahead of now extends the bound to it plus slack, one not
// ahead does not, the ceiling caps it, and the bound never moves earlier.
func TestQuietBoundFollowsRowTimersArmedInQuiescence(t *testing.T) {
	now, slack := plannerT0, time.Minute
	bound, ceiling := now.Add(2*time.Minute), now.Add(17*time.Minute)
	for _, c := range []struct {
		name         string
		bound, timer time.Time
		want         time.Time
	}{
		{"a churn clear five minutes out", bound, now.Add(5 * time.Minute), now.Add(6 * time.Minute)},
		{"a timer not ahead of now, the bound already passed", now.Add(-time.Minute), now, now.Add(-time.Minute)},
		{"a timer past the ceiling", bound, now.Add(time.Hour), ceiling},
		{"a timer whose bound would be earlier", bound, now.Add(30 * time.Second), bound},
	} {
		if got := quietBound(c.bound, c.timer, now, slack, ceiling); !got.Equal(c.want) {
			t.Errorf("%s: bound %s, want %s", c.name, got.Sub(now), c.want.Sub(now))
		}
	}
}

// Kills a sim attach that writes where production's ensureRunning does not,
// or leaves what it consumes (mc-946b6 seed 848; the #7765 review):
//   - a kill-fenced row, or one whose drain-ack stop is pending, gets no
//     row write and no start (ensureRunningAllows);
//   - a claimed creating row with no runtime is started and made active,
//     its claim cleared;
//   - a running session is not restarted, and its stop request stays
//     (started false); a dead one is started and its stop request cleared;
//   - an operator's hold is consumed: its timer, intent and stamp, the sleep
//     reason, slept_at and the wake request go, and the row reads active.
func TestSimAttachModelsEnsureRunning(t *testing.T) {
	var fn func(p *simProvider, name string, b beads.Bead)
	for _, op := range runtimeOps {
		if op.name == "attach-recreates" {
			fn = op.fn
		}
	}
	attach := func(t *testing.T, live bool, row beads.Bead) (beads.Bead, int) {
		t.Helper()
		var running []string
		if live {
			running = []string{row.ID}
		}
		s := scripted(t, running, row)
		l := s.legs[0]
		before, _ := l.backing.Get(row.ID)
		starts := len(s.sp.starts)
		s.runtimeOp("attach-recreates", fn, l, before)
		after, _ := l.backing.Get(row.ID)
		return after, len(s.sp.starts) - starts
	}
	now := rowAt(0)
	stopReq := []string{session.DrainIntentReasonKey, "orphaned", session.DrainIntentAtKey, now, session.DrainIntentIncarnationKey, "1", session.OperatorDrainKey, "operator"}

	for name, row := range map[string]beads.Bead{
		"kill-fenced":            poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-1", "state_reason", session.KillPendingReason, "sleep_reason", "killed", "slept_at", now),
		"drain-ack stop pending": poolRow("gc-1", "worker", 1, string(session.StateDraining), "instance_token", "tok-1", "state_reason", session.DrainAckStopPendingReason),
	} {
		want := maps.Clone(row.Metadata) // the store shares row's map: compare with a copy
		after, starts := attach(t, false, row)
		if !maps.Equal(after.Metadata, want) || starts != 0 {
			t.Errorf("%s: row %v, %d starts; want no write and no start", name, after.Metadata, starts)
		}
	}

	after, starts := attach(t, false, poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-1", "pending_create_claim", "true", "pending_create_started_at", now))
	if m := after.Metadata; starts != 1 || m["state"] != string(session.StateActive) || m["pending_create_claim"] != "" || m["pending_create_started_at"] != "" {
		t.Errorf("a claimed creating row: row %v, %d starts; want started, active, its claim cleared", m, starts)
	}

	after, starts = attach(t, true, poolRow("gc-1", "worker", 1, "active", append([]string{"instance_token", "tok-1"}, stopReq...)...))
	if m := after.Metadata; starts != 0 || m[session.DrainIntentReasonKey] != "orphaned" || m[session.OperatorDrainKey] != "operator" {
		t.Errorf("a running session: row %v, %d starts; want no restart and its stop request kept", m, starts)
	}
	after, starts = attach(t, false, poolRow("gc-1", "worker", 1, "asleep", append([]string{"instance_token", "tok-1"}, stopReq...)...))
	if m := after.Metadata; starts != 1 || m[session.DrainIntentReasonKey] != "" || m[session.OperatorDrainKey] != "" {
		t.Errorf("a dead session: row %v, %d starts; want started, its stop request and operator_drain cleared", m, starts)
	}

	held := poolRow("gc-1", "worker", 1, "suspended", "instance_token", "tok-1", "sleep_intent", "user-hold", "held_until", rowAt(1000*time.Hour),
		"suspended_at", rowAt(-time.Minute), "sleep_reason", "user-hold", "slept_at", now, "wake_request", "operator", "wake_requested_at", now)
	after, _ = attach(t, false, held)
	m := after.Metadata
	for _, k := range []string{"sleep_intent", "held_until", "suspended_at", "sleep_reason", "slept_at", "wake_request", "wake_requested_at"} {
		if m[k] != "" {
			t.Errorf("an operator-held row: %s = %q after the attach, want consumed", k, m[k])
		}
	}
	if m["state"] != string(session.StateActive) {
		t.Errorf("an operator-held row: state %q, want active", m["state"])
	}

	// A held row ensureRunning only confirms (mc-37xy0): asleep with its
	// runtime running (liveUserHoldDecision consumes only an active or awake
	// one), so the row reads active and the hold stands; drained and running,
	// a state the confirm leaves alone, gets no write. A held draining row
	// whose runtime is gone is started, and the Resume cell refuses (the
	// drain's stop owns the runtime), so consumeUserHold writes nothing.
	hold := []string{"instance_token", "tok-1", "sleep_intent", "user-hold", "held_until", rowAt(1000 * time.Hour), "suspended_at", rowAt(-time.Minute)}
	after, _ = attach(t, true, poolRow("gc-1", "worker", 1, "asleep", hold...))
	if m := after.Metadata; m["state"] != string(session.StateActive) || m["sleep_intent"] != "user-hold" || m["held_until"] == "" || m["suspended_at"] == "" {
		t.Errorf("a held asleep row with its runtime running: row %v, want active with the hold kept", m)
	}
	for _, c := range []struct {
		name  string
		live  bool
		state string
	}{
		{"a held drained row with its runtime running", true, string(session.StateDrained)},
		{"a held draining row with its runtime gone", false, string(session.StateDraining)},
	} {
		row := poolRow("gc-1", "worker", 1, c.state, hold...)
		want := maps.Clone(row.Metadata)
		if after, _ = attach(t, c.live, row); !maps.Equal(after.Metadata, want) {
			t.Errorf("%s: row %v, want no write", c.name, after.Metadata)
		}
	}
}

// paneAttached reports whether row's runtime name holds an attached pane.
func (s *sim) paneAttached(row beads.Bead) bool {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	rt := s.sp.rts[row.Metadata["session_name"]]
	return rt != nil && rt.attached
}
