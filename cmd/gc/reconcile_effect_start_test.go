package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The start effect's tests (CONTRACT v5 S1, S2, O1, O4; plan C5a1) over a
// scripted leaf: a fresh, corpse-aware read, a batched identity read, and a
// Start spy.

var startT0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

var (
	liveAlive  = runtime.Liveness{Running: true, Alive: true}
	liveCorpse = runtime.Liveness{Corpse: true}
	liveZombie = runtime.Liveness{Running: true}
)

// startLeaf is a hardened leaf whose runtimes are scripted per name. Its
// fresh read answers live; its plain read answers cached when set, as the
// 2-second state cache would. A Start records its config and brings up a
// runtime reading afterStart (alive when nil) under the config's env, unless
// startErr is set.
type startLeaf struct {
	*runtime.Fake
	mu         sync.Mutex
	live       map[string]runtime.Liveness
	cached     map[string]runtime.Liveness
	env        map[string]map[string]string
	afterStart *runtime.Liveness
	startErr   error
	readErr    error
	starts     []runtime.Config
	since      []time.Time
	// onIdentity runs at each identity read: between the fresh read and the
	// commit, where an out-of-process writer can land.
	onIdentity func()
	// onRead runs at each fresh read; onStart inside each Start, with its
	// context.
	onRead  func()
	onStart func(context.Context)
}

func newStartLeaf() *startLeaf {
	return &startLeaf{Fake: runtime.NewFake(), live: map[string]runtime.Liveness{}, cached: map[string]runtime.Liveness{}, env: map[string]map[string]string{}}
}

// runtimeAs puts a runtime under s-a carrying id and token at epoch 3.
func (l *startLeaf) runtimeAs(live runtime.Liveness, id, token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live["s-a"] = live
	l.env["s-a"] = map[string]string{"GC_SESSION_ID": id, "GC_INSTANCE_TOKEN": token, "GC_RUNTIME_EPOCH": "3"}
}

func (l *startLeaf) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.cached[name]; ok {
		return c, nil
	}
	return l.live[name], l.readErr
}

func (l *startLeaf) ObserveLivenessSince(name string, _ []string, since time.Time) (runtime.Liveness, error) {
	if l.onRead != nil {
		l.onRead()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.since = append(l.since, since)
	return l.live[name], l.readErr
}

func (l *startLeaf) GetAllEnvironment(name string) (map[string]string, error) {
	if l.onIdentity != nil {
		l.onIdentity()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.env[name], nil
}

func (l *startLeaf) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if l.onStart != nil {
		l.onStart(ctx)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts = append(l.starts, cfg)
	if l.startErr != nil {
		return l.startErr
	}
	l.live[name], l.env[name] = liveAlive, cfg.Env
	if l.afterStart != nil {
		l.live[name] = *l.afterStart
	}
	return nil
}

// startFixture is one row on a stamped store, its runtime leaf and clock.
type startFixture struct {
	store beads.Store
	key   rowKey
	leaf  *startLeaf
	clk   *fakePlannerClock
	tp    TemplateParams
	guard *endpointCapacityGuard
	alloc *allocDecision
	// cityPath, cfg and demand are the pass's: t.Name(), an empty city and
	// no demand unless a test sets them.
	cityPath string
	cfg      *config.City
	demand   demandView
}

// requireMem is a MemStore stamped require.
func requireMem(t *testing.T) *beads.MemStore {
	t.Helper()
	m := beads.NewMemStore()
	if err := beads.StampOpenedStore(m, "MemStore", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	return m
}

// newStartFixture seeds row s-a at generation 3 holding token tok, with meta
// over it.
func newStartFixture(t *testing.T, store beads.Store, meta ...string) *startFixture {
	t.Helper()
	base := []string{"template", "worker", "session_name", "s-a", "generation", "3", "instance_token", "tok", "state", "asleep"}
	b, err := store.Create(sessionRow("a", append(base, meta...)...))
	if err != nil {
		t.Fatal(err)
	}
	return &startFixture{
		store: store, key: rowKey{Leg: rowLeg, ID: b.ID}, leaf: newStartLeaf(), clk: newFakePlannerClock(startT0),
		tp:    TemplateParams{TemplateName: "worker", SessionName: "s-a", Command: "agent"},
		alloc: &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{}}},
	}
}

// pass is the effect pass of a pass over the store as it reads now.
func (f *startFixture) pass(t *testing.T) *effectPass {
	t.Helper()
	if f.cityPath == "" {
		f.cityPath = t.Name()
	}
	if f.cfg == nil {
		f.cfg = &config.City{}
	}
	w := &World{Now: f.clk.Now(), CityPath: f.cityPath, Env: &reconcileEnv{Cfg: f.cfg}, Census: readCensus(t, f.clk.Now(), censusLegs(rowLeg, f.store)), Demand: f.demand}
	w.LegStores = map[string]beads.Store{rowLeg: f.store}
	if row, ok := w.Census.Rows[f.key]; ok {
		w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: f.tp}}}
	}
	p := newEffectPass(w, f.alloc)
	p.Runtime, p.Start = f.leaf, startEnv{Clock: f.clk, Capacity: f.guard, Stderr: io.Discard}
	return p
}

// run runs kind's registered effect on the row.
func (f *startFixture) run(t *testing.T, kind string) settlement {
	t.Helper()
	it := intent{Kind: kind, Key: f.key, Deadline: f.clk.Now().Add(70 * time.Second)}
	return effectRegistry[kind](f.pass(t), it)(context.Background())
}

func (f *startFixture) meta(t *testing.T) map[string]string {
	t.Helper()
	b, err := f.store.Get(f.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata
}

func (f *startFixture) set(t *testing.T, kv map[string]string) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.key.ID, kv); err != nil {
		t.Error(err)
	}
}

// Kills a Launch over an alive runtime or a corpse, an adopt that launches,
// and a commit on any verdict but Current: one case per row of v5 S1's
// table.
func TestStartResolutionTable(t *testing.T) {
	creating, active, awake := session.Info{MetadataState: "creating"}, session.Info{MetadataState: "active"}, session.Info{MetadataState: "awake"}
	alive := func(v identityVerdict) freshRead { return freshRead{Class: freshAlive, Verdict: v} }
	for _, c := range []struct {
		name  string
		adopt bool
		read  freshRead
		row   session.Info
		verb  startVerb
		cause string
	}{
		{"unsupported leaf", false, freshRead{Class: freshUnsupported}, creating, verbRefuse, causeLivenessUnsupported},
		{"unknown", false, freshRead{Class: freshUnknown}, creating, verbRefuse, causeLivenessUnknown},
		{"gone launches", false, freshRead{Class: freshGone}, active, verbLaunch, ""},
		{"gone never adopts", true, freshRead{Class: freshGone}, creating, verbRefuse, causeNotPresent},
		{"current committed active", false, alive(identityCurrent), active, verbNoop, causeAlreadyRunning},
		{"current committed awake", true, alive(identityCurrent), awake, verbNoop, causeAlreadyRunning},
		{"current uncommitted commits", false, alive(identityCurrent), creating, verbCommit, ""},
		{"current uncommitted adopts", true, alive(identityCurrent), creating, verbCommit, ""},
		{"stale self", false, alive(identityStaleSelf), creating, verbRefuse, causeTokenDrift},
		{"newer self", false, alive(identityNewerSelf), creating, verbRefuse, causeNewerSelf},
		{"foreign", false, alive(identityForeign), creating, verbRefuse, causeOccupied},
		{"unknown identity", false, alive(identityUnknown), creating, verbRefuse, causeAttribution},
		{"ownerless", true, alive(identityOwnerless), creating, verbRefuse, causeAttribution},
		{"dead, even with this token", false, freshRead{Class: freshDead, Verdict: identityCurrent}, creating, verbRefuse, causeDead},
	} {
		t.Run(c.name, func(t *testing.T) {
			if verb, cause := resolveStart(c.adopt, c.read, c.row); verb != c.verb || cause != c.cause {
				t.Fatalf("resolveStart = (%d, %q), want (%d, %q)", verb, cause, c.verb, c.cause)
			}
		})
	}
}

// Kills a fresh read taken as gone or alive when it is not: a corpse and a
// zombie read dead, an errored read unknown, a leaf without an error-bearing
// read unsupported; each refuses, launching nothing and writing nothing.
func TestStartFreshReadClasses(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(l *startLeaf)
		sp    func(l *startLeaf) runtime.Provider
		cause string
	}{
		{"corpse", func(l *startLeaf) { l.runtimeAs(liveCorpse, "", "") }, nil, causeDead},
		{"zombie", func(l *startLeaf) { l.runtimeAs(liveZombie, "", "tok") }, nil, causeDead},
		{"read error", func(l *startLeaf) { l.readErr = runtime.ErrRuntimeUnavailable }, nil, causeLivenessUnknown},
		{"bool leaf", func(*startLeaf) {}, func(l *startLeaf) runtime.Provider { return boolLeaf{l.Fake} }, causeLivenessUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStartFixture(t, requireMem(t))
			c.setup(f.leaf)
			p := f.pass(t)
			if c.sp != nil {
				p.Runtime = c.sp(f.leaf)
			}
			s := effectRegistry[intentAdopt](p, intent{Kind: intentAdopt, Key: f.key})(context.Background())
			if s.Outcome != settledRefused || s.Cause != c.cause {
				t.Fatalf("settlement %+v, want refused with cause %q", s, c.cause)
			}
			if len(f.leaf.starts) != 0 || f.meta(t)["started_config_hash"] != "" {
				t.Fatal("a refusal started or wrote")
			}
		})
	}
}

// Kills a commit or an adopt that skips the identity read, and an adopt
// commit that records session.woke (legacy's heal records none): an
// uncommitted row whose runtime is alive with its token commits, notes the
// read, and records no event; the runtime's own token never changes.
func TestAdoptCommitsAliveCurrentUncommittedRow(t *testing.T) {
	for _, backend := range rowWriteBackends {
		t.Run(backend.name, func(t *testing.T) {
			f := newStartFixture(t, backend.open(t), "state", "creating", "pending_create_claim", "true")
			f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
			f.clk.Advance(time.Second)
			s := f.run(t, intentAdopt)
			if s.Outcome != settledLanded || s.Event != nil || s.Noted == nil || s.Noted.Name != "s-a" || !s.Noted.At.Equal(f.clk.Now()) {
				t.Fatalf("settlement %+v, want landed, noted at the read, with no event", s)
			}
			m := f.meta(t)
			if m["state"] != "active" || m["pending_create_claim"] != "" || m["started_config_hash"] == "" || m["instance_token"] != "tok" {
				t.Fatalf("row after the adopt = %v, want active, claim cleared, hashed, token kept", m)
			}
			if len(f.leaf.starts) != 0 {
				t.Fatal("an adopt launched")
			}
		})
	}
}

// Kills committing a runtime that does not carry the row's token (I3): the
// row's ID with another token, another row's ID, and no identity each
// refuse, and the row is not written.
func TestCommitRequiresRuntimeToCarryIntentToken(t *testing.T) {
	for _, c := range []struct {
		name, id, token, cause string
	}{
		{"stale self", "", "older", causeTokenDrift},
		{"foreign", "other-row", "theirs", causeOccupied},
		{"ownerless", "-", "", causeAttribution},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStartFixture(t, requireMem(t), "state", "creating")
			id := c.id
			switch id {
			case "":
				id = f.key.ID
			case "-":
				id = ""
			}
			f.leaf.runtimeAs(liveAlive, id, c.token)
			if s := f.run(t, intentAdopt); s.Outcome != settledRefused || s.Cause != c.cause {
				t.Fatalf("settlement %+v, want refused with cause %q", s, c.cause)
			}
			if m := f.meta(t); m["state"] != "creating" || m["started_config_hash"] != "" {
				t.Fatalf("row %v was written", m)
			}
		})
	}
}

// Kills CommitStartedIfCurrent's premise (v5 S2): what an out-of-process
// writer lands between the fresh read and the commit decides it. A kill
// fence (gc session kill writes asleep; scenario R29b), another token or a
// closed row refuses with cause commit-lost and writes nothing; a row the
// CLI confirmed active with the same token still commits; a hold does not
// veto.
func TestCommitPremise(t *testing.T) {
	for _, c := range []struct {
		name   string
		write  map[string]string
		landed bool
	}{
		{"kill fence after PreWake R29b", map[string]string{"state": "asleep", "sleep_reason": "killed"}, false},
		{"another token", map[string]string{"instance_token": "newer"}, false},
		{"CLI confirmed active", map[string]string{"state": "active", "state_reason": "creation_complete"}, true},
		{"held", map[string]string{"held_until": startT0.Add(time.Hour).Format(time.RFC3339)}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, backend := range rowWriteBackends {
				f := newStartFixture(t, backend.open(t), "state", "creating")
				f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
				f.leaf.onIdentity = func() { f.set(t, c.write) }
				s := f.run(t, intentAdopt)
				if got := s.Outcome == settledLanded; got != c.landed || (!got && s.Cause != causeCommitLost) {
					t.Fatalf("%s: settlement %+v, want landed=%v (else commit-lost)", backend.name, s, c.landed)
				}
				if hashed := f.meta(t)["started_config_hash"] != ""; hashed != c.landed {
					t.Fatalf("%s: commit wrote=%v, want %v", backend.name, hashed, c.landed)
				}
			}
		})
	}
	t.Run("closed", func(t *testing.T) {
		f := newStartFixture(t, requireMem(t), "state", "creating")
		f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
		f.leaf.onIdentity = func() {
			if err := f.store.Close(f.key.ID); err != nil {
				t.Error(err)
			}
		}
		if s := f.run(t, intentAdopt); s.Outcome != settledRefused || s.Cause != causeCommitLost {
			t.Fatalf("settlement %+v, want commit-lost", s)
		}
	})
}

// Kills a start under a name another effect or reaper holds, and a lock
// dropped before the commit: a busy name refuses with cause name-busy (a row
// backoff) before any read, and the effect holds the name at its read and
// at every store read through the commit.
func TestStartEffectHoldsNameLockThroughCommit(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "state", "creating")
	f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
	unlock := runtimeNames.tryLock(t.Name(), "s-a")
	if s := f.run(t, intentAdopt); s.Outcome != settledRefused || s.Cause != causeNameBusy || len(f.leaf.since) != 0 {
		t.Fatalf("settlement %+v after %d reads, want name-busy before any read", s, len(f.leaf.since))
	}
	unlock()

	gets := 0
	held := func() {
		if u := runtimeNames.tryLock(t.Name(), "s-a"); u != nil {
			u()
			t.Error("the name lock was free while the effect ran")
		}
	}
	f.leaf.onIdentity = held
	f.store = &getHookStore{Store: f.store, onGet: func() { gets++; held() }}
	if s := f.run(t, intentAdopt); s.Outcome != settledLanded || gets == 0 {
		t.Fatalf("settlement %+v after %d store reads, want landed", s, gets)
	}
	if u := runtimeNames.tryLock(t.Name(), "s-a"); u == nil {
		t.Fatal("the name lock outlived the effect")
	} else {
		u()
	}
}

// getHookStore runs onGet after every Get and resolves its conditional writer
// on the store it wraps.
type getHookStore struct {
	beads.Store
	onGet func()
}

func (s *getHookStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	s.onGet()
	return b, err
}

func (s *getHookStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// Kills a start decided from the 2-second cache, and a global re-list per
// effect (v5 O1): the plain read says gone while the fresh read, taken
// since the effect began and per name, says alive with this token, so the
// effect is a Noop and nothing lists.
func TestFreshTmuxReadIsPerNameNotGlobalInvalidate(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "state", "active")
	f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
	f.leaf.cached["s-a"] = runtime.Liveness{}
	began := f.clk.Now()
	s := f.run(t, intentAdopt)
	if s.Outcome != settledNoop || s.Cause != causeAlreadyRunning || s.Noted == nil {
		t.Fatalf("settlement %+v, want a noted Noop", s)
	}
	if len(f.leaf.starts) != 0 || !slices.Equal(f.leaf.since, []time.Time{began}) {
		t.Fatalf("starts %d, fresh reads since %v; want none, and one since the effect began", len(f.leaf.starts), f.leaf.since)
	}
	for _, c := range f.leaf.SnapshotCalls() {
		if c.Method == "ListRunning" {
			t.Fatal("the effect listed every runtime")
		}
	}
}

// Kills a blind write from any verb (v5 R3): every write the effect makes
// reaches the store as a CAS through its conditional writer, never as an
// unconditional Update, SetMetadata or Create on the store.
func TestStartEffectMakesNoBlindWrites(t *testing.T) {
	for _, c := range []struct{ name, state, token string }{
		{"adopt commit", "creating", "tok"},
		{"noop", "active", "tok"},
		{"refusal", "creating", "other"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStartFixture(t, requireMem(t), "state", c.state)
			f.leaf.runtimeAs(liveAlive, f.key.ID, c.token)
			f.store = blindSpyStore{Store: f.store, t: t, row: f.key.ID}
			f.run(t, intentAdopt)
		})
	}
}

// blindSpyStore fails the test on every unconditional write to the session
// row, or of a session row; other beads (a #46 episode record) pass through.
type blindSpyStore struct {
	beads.Store
	t   *testing.T
	row string
}

func (s blindSpyStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func (s blindSpyStore) blind(method, id string) bool {
	if id == s.row {
		s.t.Errorf("blind %s on the session row", method)
		return true
	}
	return false
}

func (s blindSpyStore) Update(id string, opts beads.UpdateOpts) error {
	if s.blind("Update", id) {
		return nil
	}
	return s.Store.Update(id, opts)
}

func (s blindSpyStore) SetMetadata(id, k, v string) error {
	if s.blind("SetMetadata", id) {
		return nil
	}
	return s.Store.SetMetadata(id, k, v)
}

func (s blindSpyStore) SetMetadataBatch(id string, kv map[string]string) error {
	if s.blind("SetMetadataBatch", id) {
		return nil
	}
	return s.Store.SetMetadataBatch(id, kv)
}

func (s blindSpyStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type == session.BeadType {
		s.t.Error("blind Create of a session row")
		return b, nil
	}
	return s.Store.Create(b)
}

// Kills a second start proposed while the inventory lags a commit (scenario
// R36, I2; v5 O4): the drain notes the start's fresh read, a pass that
// began before that read cannot conclude the name gone, and a pass that
// began after it decides either way.
func TestJustStartedRowNotReproposedWhileInventoryLags(t *testing.T) {
	c := observeRows(t, map[string]string{"gc-1": "s1"})
	cache := newObserveCache()
	cache.publish(censusNow, nil, completeBackend("tmux"))
	p := settlePlanner(newInflightMap())
	p.observations = func() *ObservationCache { return cache.ObservationCache }
	read := censusNow.Add(2 * time.Second)
	p.settlements.post(settlement{Key: rowKey{Leg: "class:sessions", ID: "gc-1"}, Kind: intentStart, Outcome: settledLanded, Noted: &notedRuntime{Name: "s1", At: read}})
	p.drainSettlements(read)
	for _, kind := range []FactKind{FactListed, FactRunning, FactProcessAlive} {
		obs := cache.Snapshot().ByName["s1"]
		if f := obs.fact(kind); f.Value != ObsYes || !f.ObservedAt.Equal(read) {
			t.Fatalf("fact %d after the drain = %+v, want Yes at the read", kind, *f)
		}
	}
	lagging := cache.publish(read.Add(-time.Second), nil, completeBackend("tmux"))
	if got := observed(t, lagging, c, read, "gc-1"); got.Liveness.startCandidate() {
		t.Fatalf("a pass that began before the start's read: %+v, want no start candidate", got)
	}
	later := cache.publish(read.Add(time.Second), nil, completeBackend("tmux"))
	if got := observed(t, later, c, read.Add(time.Second), "gc-1"); got.Liveness != livenessGone {
		t.Fatalf("a pass that began after the read: %+v, want gone", got)
	}
}

// Kills the transcript option leaking into legacy: legacy's prepare still
// mints a key through the store; with the option, prepare writes nothing.
func TestPrepareNilTranscriptOptionIsLegacy(t *testing.T) {
	for _, option := range []*sessTranscriptState{nil, new(sessTranscriptState)} {
		store := beads.NewMemStore()
		b, err := store.Create(sessionRow("a", "template", "worker", "session_name", "s-a", "instance_token", "tok"))
		if err != nil {
			t.Fatal(err)
		}
		tp := TemplateParams{TemplateName: "worker", Command: "agent", ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}
		info := sessionInfoOf(t, store, b.ID)
		candidate := startCandidate{info: info, tp: tp}
		if option == nil {
			_, _, err = buildPreparedStartWithWorkDirResolver(candidate, "", &config.City{}, store, nil)
		} else {
			_, _, err = buildPreparedStartWithTranscript(candidate, "", &config.City{}, store, nil, option)
		}
		if err != nil {
			t.Fatal(err)
		}
		if minted := sessionInfoOf(t, store, b.ID).SessionKey != ""; minted != (option == nil) {
			t.Fatalf("option %v: minted=%v, want only legacy to mint", option, minted)
		}
	}
}

func sessionInfoOf(t *testing.T, store beads.Store, id string) session.Info {
	t.Helper()
	info, err := sessionFrontDoor(store).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// Kills a commit that outlives its context: a cancel landing between the
// fresh read and the commit settles as a deadline, by context.Cause, and
// writes nothing.
func TestCommitRefusesContextEndedAfterRead(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "state", "creating")
	f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
	ctx, cancel := context.WithCancelCause(context.Background())
	f.leaf.onIdentity = func() { cancel(context.DeadlineExceeded) }
	it := intent{Kind: intentAdopt, Key: f.key}
	if s := adoptEffect(f.pass(t), it)(ctx); s.Outcome != settledFailed || s.Cause != causeDeadline {
		t.Fatalf("settlement %+v, want failed at the deadline", s)
	}
	if f.meta(t)["started_config_hash"] != "" {
		t.Fatal("committed after the context ended")
	}
}

// Kills a Note stamped at the commit or at the drain instead of at the read
// (v5 O4): a clock that moves on during the identity read leaves the noted
// time at the read's issue, and the drain writes that time, not its own.
func TestNoteStampedAtReadNotDrain(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "state", "creating")
	f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
	read := f.clk.Now()
	f.leaf.onIdentity = func() { f.clk.Advance(time.Minute) }
	s := f.run(t, intentAdopt)
	if s.Noted == nil || !s.Noted.At.Equal(read) {
		t.Fatalf("noted %+v, want stamped at the read %s", s.Noted, read)
	}
	cache := newObserveCache()
	p := settlePlanner(newInflightMap())
	p.observations = func() *ObservationCache { return cache.ObservationCache }
	p.settlements.post(s)
	p.drainSettlements(read.Add(time.Hour))
	if obs := cache.Snapshot().ByName["s-a"]; !obs.Listed.ObservedAt.Equal(read) {
		t.Fatalf("noted at %s, want the read's %s", obs.Listed.ObservedAt, read)
	}
}

// Kills a single-shot commit CAS and contention read as a lost premise:
// writers that land between each read and its CAS are outlasted within
// commitAttempts, and past it the commit refuses with its own cause,
// commit-contended, not commit-lost.
func TestCommitRetriesContention(t *testing.T) {
	for _, c := range []struct {
		races  int
		landed bool
	}{{commitAttempts - 1, true}, {commitAttempts, false}} {
		f := newStartFixture(t, requireMem(t), "state", "creating")
		f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
		inner, n := f.store, 0
		f.store = &getHookStore{Store: inner, onGet: func() {
			if n < c.races {
				n++
				if err := inner.SetMetadata(f.key.ID, "racer", strconv.Itoa(n)); err != nil {
					t.Error(err)
				}
			}
		}}
		s := f.run(t, intentAdopt)
		if got := s.Outcome == settledLanded; got != c.landed || (!got && s.Cause != causeCommitContended) {
			t.Fatalf("%d races: settlement %+v, want landed=%v, else commit-contended", c.races, s, c.landed)
		}
	}
}

// Kills a fresh read and commit split across two lock sections: the row's
// session mutation lock is held from the fresh read through the commit, so
// a Manager path (API wake, submit) cannot write between them.
func TestStartEffectHoldsMutationLockFromReadThroughCommit(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "state", "creating")
	f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
	acquired := make(chan struct{})
	f.leaf.onIdentity = func() {
		go func() { _ = session.WithSessionMutationLock(f.key.ID, func() error { close(acquired); return nil }) }()
		select {
		case <-acquired:
			t.Error("another writer took the session mutation lock between the read and the commit")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if s := f.run(t, intentAdopt); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	<-acquired
}

// stateWriters are the functions outside the planner that write a row's
// state key, or a key named state, each with its class (SC R1): the commit
// premise (open, this token, creating/active/awake) refuses a start whenever
// one of them moves a row off those states, which only operator lifecycle
// verbs should do.
var stateWriters = map[string]string{
	"cmd/gc/city_runtime.go:CityRuntime.recordReconcileTraceInputs":           "trace payload, not a row",
	"cmd/gc/city_runtime.go:CityRuntime.recordReconcileTraceResults":          "trace payload, not a row",
	"cmd/gc/cmd_registry_auth.go:registryBrowserLogin":                        "OAuth state, not a row",
	"cmd/gc/cmd_session.go:cmdSessionSuspend":                                 "operator: gc session suspend",
	"cmd/gc/cmd_session_wake.go:doSessionWake":                                "operator: gc session wake",
	"cmd/gc/endpoint_capacity.go:endpointCapacityGuard.recordTick":            "trace payload, not a row",
	"cmd/gc/session_bead_cycle.go:cycleAliveSessionForFreshReassign":          "legacy reconciler",
	"cmd/gc/session_beads.go:reopenNamedSessionBatch":                         "legacy reconciler",
	"cmd/gc/session_identity.go:desiredSessionIdentity":                       "create's initial metadata",
	"cmd/gc/session_index.go:sessionIndex.occupancy":                          "read model, not a write",
	"cmd/gc/session_lifecycle_parallel.go:executePlannedStartsTraced":         "legacy reconciler (trace payload)",
	"cmd/gc/session_name_lookup.go:createPoolSessionBeadWithIdentifiers":      "create's initial metadata",
	"cmd/gc/session_reconcile.go:healStatePatchWithRollbackInfo":              "legacy reconciler",
	"cmd/gc/session_reconcile.go:providerTerminalErrorPatch":                  "patch builder (C5a2's abandon)",
	"cmd/gc/session_reconciler.go:reconcileSessionBeadsTracedWithNamedDemand": "legacy reconciler",
	"internal/session/chat.go:Manager.confirmLiveSessionState":                "Manager: CLI/API start confirms active",
	"internal/session/lifecycle_exits.go:RateLimitQuarantinePatch":            "patch builder",
	"internal/session/lifecycle_transition.go:AcknowledgeDrainPatch":          "patch builder",
	"internal/session/lifecycle_transition.go:ArchivePatch":                   "patch builder",
	"internal/session/lifecycle_transition.go:BeginDrainPatch":                "patch builder",
	"internal/session/lifecycle_transition.go:ClearWakeBlockersPatch":         "patch builder",
	"internal/session/lifecycle_transition.go:ClosePatch":                     "patch builder",
	"internal/session/lifecycle_transition.go:CommitStartedPatch":             "patch builder (S2's commit)",
	"internal/session/lifecycle_transition.go:ConfigDriftResetPatch":          "patch builder",
	"internal/session/lifecycle_transition.go:ConfirmStartedPatch":            "patch builder",
	"internal/session/lifecycle_transition.go:PreWakePatch":                   "patch builder (S1's PreWake)",
	"internal/session/lifecycle_transition.go:QuarantinePatch":                "patch builder",
	"internal/session/lifecycle_transition.go:ReactivatePatch":                "patch builder",
	"internal/session/lifecycle_transition.go:RequestWakePatch":               "patch builder",
	"internal/session/lifecycle_transition.go:SleepPatch":                     "patch builder (gc session kill, idle sleep)",
	"internal/session/manager.go:Manager.createBeadOnly":                      "create's initial metadata",
	"internal/session/manager.go:Manager.createStarted":                       "create's initial metadata",
	"internal/session/manager.go:Manager.suspend":                             "operator: suspend",
	"internal/session/store.go:Store.SetState":                                "operator verbs' blind setter",
	"internal/session/wait_store.go:Store.CancelWait":                         "wait bead, not a row",
	"internal/session/wait_store.go:Store.CloseWaitFromNudge":                 "wait bead, not a row",
	"internal/session/wait_store.go:Store.CreateWait":                         "wait bead, not a row",
	"internal/session/wait_store.go:Store.ExpireWait":                         "wait bead, not a row",
	"internal/session/wait_store.go:Store.FailWait":                           "wait bead, not a row",
	"internal/session/wait_store.go:Store.FailWaitFromNudge":                  "wait bead, not a row",
	"internal/session/wait_store.go:Store.MarkWaitReady":                      "wait bead, not a row",
	"internal/session/wait_store.go:Store.MarkWaitReadyForRedelivery":         "wait bead, not a row",
	"internal/session/wait_store.go:Store.RetryClosedWait":                    "wait bead, not a row",
	"internal/session/wait_store.go:Store.cancelWaitsAndCollectNudgeIDs":      "wait bead, not a row",
}

// Kills an unclassified writer of state outside the planner (SC R1): every
// function in the CLI, API, Manager and legacy reconciler that writes a
// "state" key, by map literal, index assignment, SetMetadata or SetMarker,
// or calls SetState, is in stateWriters, and every entry still writes.
func TestStateWritersOutsideThePlannerArePinned(t *testing.T) {
	found := make(map[string]bool)
	isState := func(e ast.Expr) bool { b, ok := e.(*ast.BasicLit); return ok && b.Value == `"state"` }
	root := repoRootForLint(t)
	for _, dir := range []string{"cmd/gc", "internal/session", "internal/api", "internal/worker"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources under %s (%v)", dir, err)
		}
		for _, path := range files {
			rel, base := dir+"/"+filepath.Base(path), filepath.Base(path)
			if strings.HasSuffix(base, "_test.go") || dir == "cmd/gc" && (strings.HasPrefix(base, "reconcile_") || strings.HasPrefix(base, "allocator_")) {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					hit := false
					switch n := n.(type) {
					case *ast.KeyValueExpr:
						hit = isState(n.Key)
					case *ast.AssignStmt:
						for _, l := range n.Lhs {
							ix, ok := l.(*ast.IndexExpr)
							hit = hit || ok && isState(ix.Index)
						}
					case *ast.CallExpr:
						if sel, ok := n.Fun.(*ast.SelectorExpr); ok && len(n.Args) >= 2 {
							hit = sel.Sel.Name == "SetState" || (sel.Sel.Name == "SetMetadata" || sel.Sel.Name == "SetMarker") && isState(n.Args[1])
						}
					}
					if hit {
						found[rel+":"+funcDeclName(fn)] = true
					}
					return true
				})
			}
		}
	}
	for w := range found {
		if _, ok := stateWriters[w]; !ok {
			t.Errorf("unclassified writer of state: %s (classify it in stateWriters; the start commit's premise depends on it)", w)
		}
	}
	for w := range stateWriters {
		if !found[w] {
			t.Errorf("stateWriters lists %s, which no longer writes state: drop it", w)
		}
	}
}

// concretePoolFixture is a concrete pool row (alias gastown.capable of
// template gastown.polecat) whose work_dir still names its template's
// directory, with meta over it; it returns the fixture and the concrete dir.
func concretePoolFixture(t *testing.T, meta ...string) (*startFixture, string) {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repos", "aot-mobile")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	templateDir := filepath.Join(cityPath, ".gc", "worktrees", "aot-mobile", "polecats", "gastown.polecat")
	concreteDir := filepath.Join(cityPath, ".gc", "worktrees", "aot-mobile", "polecats", "gastown.capable")
	f := newStartFixture(t, requireMem(t), append([]string{
		"template", "aot-mobile/gastown.polecat", "agent_name", "aot-mobile/gastown.capable",
		"alias", "aot-mobile/gastown.capable", "pool_slot", "1", poolManagedMetadataKey, boolMetadata(true),
		beadmeta.WorkDirMetadataKey, templateDir, beadmeta.LegacyWorkDirMetadataKey, templateDir,
	}, meta...)...)
	f.cityPath = cityPath
	f.cfg = &config.City{
		Workspace: config.Workspace{Name: "aot"}, Rigs: []config.Rig{{Name: "aot-mobile", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "gastown.polecat", Dir: "aot-mobile", Provider: "test-agent", StartCommand: "true",
			WorkDir: ".gc/worktrees/{{.Rig}}/polecats/{{.AgentBase}}", NamepoolNames: []string{"gastown.capable"},
		}},
	}
	f.tp = TemplateParams{TemplateName: "aot-mobile/gastown.polecat", InstanceName: "aot-mobile/gastown.capable", SessionName: "s-a", WorkDir: concreteDir}
	return f, concreteDir
}

// Kills a prepare that writes (S-1): an adopt over a concrete pool row whose
// work_dir needs repair commits, with prepare folding the repair in memory
// only (the refusing store would fail a write) and nothing blind reaching
// the row.
func TestAdoptPrepareWritesNothing(t *testing.T) {
	f, _ := concretePoolFixture(t, "state", "creating")
	f.leaf.runtimeAs(liveAlive, f.key.ID, "tok")
	templateDir := f.meta(t)[beadmeta.WorkDirMetadataKey]
	f.store = blindSpyStore{Store: f.store, t: t, row: f.key.ID}
	if s := f.run(t, intentAdopt); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if got := f.meta(t)[beadmeta.WorkDirMetadataKey]; got != templateDir {
		t.Fatalf("work_dir %q, want the adopt to leave it (the row-metadata verb repairs it)", got)
	}
}

// staleRoute is a composite whose route names leaf, while the runtime
// answers alive on its other backend.
type staleRoute struct {
	*runtime.Fake
	leaf runtime.Provider
}

func (s staleRoute) RouteFor(string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: "tmux", Provider: s.leaf}, Known: true}
}

func (staleRoute) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return liveAlive, nil
}

// Kills presence read on the composite paired with identity read on the
// leaf: with the routed backend gone and the other backend alive, a
// leftover sidecar on the leaf carrying the row's token is not taken as
// this row's runtime, and the gone leaf is not taken as absence. The
// effect refuses, adopting nothing and launching nothing.
func TestStaleRouteNeitherAdoptsNorReadsGone(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "state", "creating")
	f.leaf.runtimeAs(runtime.Liveness{}, f.key.ID, "tok") // gone, its sidecar left behind
	p := f.pass(t)
	p.Runtime = staleRoute{Fake: runtime.NewFake(), leaf: f.leaf}
	s := adoptEffect(p, intent{Kind: intentAdopt, Key: f.key})(context.Background())
	if s.Outcome != settledRefused || s.Cause != causeLivenessUnknown || f.meta(t)["started_config_hash"] != "" {
		t.Fatalf("settlement %+v, row %v; want refused liveness-unknown and nothing committed", s, f.meta(t))
	}
}
