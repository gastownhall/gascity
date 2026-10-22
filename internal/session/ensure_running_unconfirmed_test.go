package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// freshAnswer is one scripted fresh liveness read.
type freshAnswer struct {
	live runtime.Liveness
	err  error
}

// cachingFreshProvider models a provider whose IsRunning answers from a
// snapshot (tmux's StateCache) while ObserveLivenessSince reads the runtime
// fresh: cached is the snapshot's answer, answers the fresh reads in order
// (the last repeats), observed how many were taken. A fresh read that finds
// nothing running after the manager's own Start also takes the fake session
// down, as a resumed process that died on its stale key does.
type cachingFreshProvider struct {
	*runtime.Fake
	cached     bool
	answers    []freshAnswer
	observed   int
	managerRan bool
}

func (p *cachingFreshProvider) IsRunning(string) bool { return p.cached }

func (p *cachingFreshProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.managerRan = true
	return p.Fake.Start(ctx, name, cfg)
}

func (p *cachingFreshProvider) ObserveLivenessSince(name string, _ []string, _ time.Time) (runtime.Liveness, error) {
	i := min(p.observed, len(p.answers)-1)
	p.observed++
	a := p.answers[i]
	if a.err == nil && !a.live.Running && p.managerRan {
		_ = p.Stop(name)
	}
	return a.live, a.err
}

var (
	freshRunning = freshAnswer{live: runtime.Liveness{Running: true, Alive: true}}
	freshAbsent  = freshAnswer{}
	freshCorpse  = freshAnswer{live: runtime.Liveness{Corpse: true}}
	freshUnknown = freshAnswer{err: fmt.Errorf("tmux transport: %w", runtime.ErrRuntimeUnavailable)}
)

const unconfirmedResumeCmd = "claude --dangerously --resume " + capacityTestSessionKey

// seedUnconfirmedSession stores a resume-capable session bead in state and
// returns its manager over a cachingFreshProvider scripted with cached and
// answers. keyed gives the bead a conversation key, which arms the stale-key
// check after a successful Start.
func seedUnconfirmedSession(t *testing.T, state State, keyed, cached bool, answers ...freshAnswer) (*Manager, *cachingFreshProvider, beads.Store, string, string) {
	t.Helper()
	return seedUnconfirmedSessionVia(t, nil, state, keyed, cached, answers...)
}

// seedUnconfirmedSessionVia is seedUnconfirmedSession with the manager over
// wrap(the caching provider), a composite in front of it; nil is the provider
// itself.
func seedUnconfirmedSessionVia(t *testing.T, wrap func(*cachingFreshProvider) runtime.Provider, state State, keyed, cached bool, answers ...freshAnswer) (*Manager, *cachingFreshProvider, beads.Store, string, string) {
	t.Helper()
	store := beads.NewMemStore()
	sp := &cachingFreshProvider{Fake: runtime.NewFake(), cached: cached, answers: answers}
	var front runtime.Provider = sp
	if wrap != nil {
		front = wrap(sp)
	}
	mgr := newTestManager(t, store, front, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	meta := map[string]string{
		"state":           string(state),
		"provider":        "claude",
		"work_dir":        "/tmp",
		"command":         "claude --dangerously",
		"resume_flag":     "--resume",
		"session_id_flag": "--session-id",
	}
	if keyed {
		meta["session_key"] = capacityTestSessionKey
		meta["started_config_hash"] = capacityTestConfigHash
	}
	b, err := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}
	sessName := sessionNameFor(b.ID)
	if err := store.SetMetadata(b.ID, "session_name", sessName); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}
	return mgr, sp, store, b.ID, sessName
}

func callCount(sp *runtime.Fake, method string) int {
	n := 0
	for _, c := range sp.Calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// metadataOf is the row's metadata without the runtime lease's own record
// (runtime_lease_*): a start takes the lease before it reads, so those keys
// move on every attempt, including one that starts nothing.
func metadataOf(t *testing.T, store beads.Store, id string) map[string]string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	meta := maps.Clone(b.Metadata)
	maps.DeleteFunc(meta, func(k, _ string) bool { return strings.HasPrefix(k, "runtime_lease_") })
	return meta
}

// converged reports err as a start path's convergence on a runtime that is
// already up: nil, or for the reconciler's runtime-only bridge also the
// session-exists error its Start got.
func converged(entry string, err error) bool {
	if entry == "StartRuntimeOnly" && errors.Is(err, runtime.ErrSessionExists) {
		return !errors.Is(err, runtime.ErrRuntimeUnavailable)
	}
	return err == nil
}

// startEntry is each manager entry that brings a session up: the operator's
// Start and the reconciler's runtime-only bridge.
// Each is a factory: the runtime-only bridge runs under one held lease, so a
// test that starts twice builds the starter once.
var startEntry = map[string]func(t *testing.T, mgr *Manager, id string) func() error{
	"Start": func(_ *testing.T, mgr *Manager, id string) func() error {
		by := testActor(mgr, ActorOperator)
		return func() error {
			return mgr.Start(context.Background(), by, id, unconfirmedResumeCmd, runtime.Config{WorkDir: "/tmp"})
		}
	},
	"StartRuntimeOnly": func(t *testing.T, mgr *Manager, id string) func() error {
		by := runtimeOnlyActor(t, mgr, id)
		inc := mintUnder(t, mgr, by.Lease, id)
		return func() error {
			return mgr.StartRuntimeOnly(context.Background(), by, id, inc, unconfirmedResumeCmd, runtime.Config{WorkDir: "/tmp"})
		}
	},
}

// A snapshot that says running is believed: no fresh read, no Start.
func TestEnsureRunning_CachedRunningTakesNoFreshReadAndStartsNothing(t *testing.T) {
	for entry, newStart := range startEntry {
		t.Run(entry, func(t *testing.T) {
			mgr, sp, _, id, _ := seedUnconfirmedSession(t, StateAsleep, false, true, freshUnknown)
			if err := newStart(t, mgr, id)(); err != nil {
				t.Fatalf("%s: %v", entry, err)
			}
			if sp.observed != 0 {
				t.Errorf("fresh reads = %d, want 0 behind a running snapshot", sp.observed)
			}
			if got := callCount(sp.Fake, "Start"); got != 0 {
				t.Errorf("Start called %d times, want 0", got)
			}
		})
	}
}

// A stale "absent" snapshot over a live runtime converges instead of
// starting a second one or sweeping the live one as an orphan.
func TestEnsureRunning_StaleAbsentSnapshotOverLiveRuntimeConverges(t *testing.T) {
	for entry, newStart := range startEntry {
		t.Run(entry, func(t *testing.T) {
			mgr, sp, _, id, _ := seedUnconfirmedSession(t, StateAsleep, false, false, freshRunning)
			if err := newStart(t, mgr, id)(); err != nil {
				t.Fatalf("%s: %v", entry, err)
			}
			if sp.observed != 1 {
				t.Errorf("fresh reads = %d, want 1", sp.observed)
			}
			if got := callCount(sp.Fake, "Start"); got != 0 {
				t.Errorf("Start called %d times, want 0", got)
			}
			if got := callCount(sp.Fake, "FindRuntimesBySessionID"); got != 0 {
				t.Errorf("orphan scans = %d, want 0 for a runtime that is up", got)
			}
		})
	}
}

// An unreadable runtime is neither running nor absent: the start path
// refuses before it starts, scans for orphans or writes anything.
func TestEnsureRunning_UnconfirmedLivenessStartsNothingAndChangesNothing(t *testing.T) {
	for entry, newStart := range startEntry {
		t.Run(entry, func(t *testing.T) {
			mgr, sp, store, id, _ := seedUnconfirmedSession(t, StateAsleep, true, false, freshUnknown)
			start := newStart(t, mgr, id) // the bridge's mint is its caller's, before the start
			before := metadataOf(t, store, id)

			err := start()

			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, want runtime unavailable", entry, err)
			}
			if got := callCount(sp.Fake, "Start"); got != 0 {
				t.Errorf("Start called %d times, want 0", got)
			}
			if got := callCount(sp.Fake, "FindRuntimesBySessionID"); got != 0 {
				t.Errorf("orphan scans = %d, want 0: an unconfirmed runtime may be the live one", got)
			}
			if after := metadataOf(t, store, id); !maps.Equal(before, after) {
				t.Errorf("bead metadata changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// A confirmed absence still starts exactly once, and so does a corpse: a
// listed session with no live pane is not running.
func TestEnsureRunning_ConfirmedNotRunningStartsOnce(t *testing.T) {
	for entry, newStart := range startEntry {
		for name, answer := range map[string]freshAnswer{"absent": freshAbsent, "corpse": freshCorpse} {
			t.Run(entry+"/"+name, func(t *testing.T) {
				mgr, sp, _, id, _ := seedUnconfirmedSession(t, StateAsleep, false, false, answer)
				if err := newStart(t, mgr, id)(); err != nil {
					t.Fatalf("%s: %v", entry, err)
				}
				if got := callCount(sp.Fake, "Start"); got != 1 {
					t.Errorf("Start called %d times, want 1", got)
				}
			})
		}
	}
}

// Start landed, the next read of the runtime is unknown: the session may be
// up, so there is no fresh retry, no second Start, and the resume identity
// stays. The next attempt converges on what is there.
func TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessKeepsResumeState(t *testing.T) {
	for entry, newStart := range startEntry {
		t.Run(entry, func(t *testing.T) {
			// Each entry reads at its gate first (Start under its lease, the
			// bridge before it starts), then once more for the stale-key check.
			mgr, sp, store, id, _ := seedUnconfirmedSession(t, StateSuspended, true, false, freshAbsent, freshUnknown)
			before := metadataOf(t, store, id)
			start := newStart(t, mgr, id)

			err := start()

			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, want runtime unavailable", entry, err)
			}
			if got := callCount(sp.Fake, "Start"); got != 1 {
				t.Errorf("Start called %d times, want 1 (no fresh retry)", got)
			}
			if got := callCount(sp.Fake, "FindRuntimesBySessionID"); got != 1 {
				t.Errorf("orphan scans = %d, want only the one before the first Start", got)
			}
			after := metadataOf(t, store, id)
			for _, k := range []string{"session_key", "started_config_hash", "continuation_reset_pending"} {
				if after[k] != before[k] {
					t.Errorf("%s = %q, want %q kept", k, after[k], before[k])
				}
			}

			sp.answers = []freshAnswer{freshRunning}
			sp.observed = 0
			if err := start(); !converged(entry, err) {
				t.Fatalf("%s once the runtime reads running: %v, want convergence", entry, err)
			}
			if got := metadataOf(t, store, id)["session_key"]; got != capacityTestSessionKey {
				t.Errorf("session_key = %q after converging, want %q kept", got, capacityTestSessionKey)
			}
		})
	}
}

// A confirmed absence after a successful Start is still the stale-key shape.
func TestEnsureRunning_StaleKeyCheckOnConfirmedAbsenceStillRetriesFresh(t *testing.T) {
	for entry, newStart := range startEntry {
		t.Run(entry, func(t *testing.T) {
			mgr, sp, store, id, _ := seedUnconfirmedSession(t, StateSuspended, true, false, freshAbsent)
			if err := newStart(t, mgr, id)(); err != nil {
				t.Fatalf("%s: %v", entry, err)
			}
			if got := callCount(sp.Fake, "Start"); got != 2 {
				t.Errorf("Start called %d times, want the resume plus one fresh retry", got)
			}
			if got := metadataOf(t, store, id)["session_key"]; got == capacityTestSessionKey {
				t.Errorf("session_key = %q, want the stale key cleared", got)
			}
		})
	}
}

// Start says the session exists. Whether that is convergence depends on a
// fresh read: running converges, unknown is an error that is both.
func TestEnsureRunning_SessionExistsConvergesOnFreshRead(t *testing.T) {
	for entry, newStart := range startEntry {
		t.Run(entry+"/running", func(t *testing.T) {
			mgr, sp, _, id, sessName := seedUnconfirmedSession(t, StateAsleep, false, false, freshAbsent, freshRunning)
			if err := sp.Fake.Start(context.Background(), sessName, runtime.Config{}); err != nil {
				t.Fatalf("seed runtime: %v", err)
			}
			if err := newStart(t, mgr, id)(); !converged(entry, err) {
				t.Fatalf("%s: %v, want convergence on the running session", entry, err)
			}
		})
		t.Run(entry+"/unknown", func(t *testing.T) {
			mgr, sp, _, id, sessName := seedUnconfirmedSession(t, StateAsleep, false, false, freshAbsent, freshUnknown)
			if err := sp.Fake.Start(context.Background(), sessName, runtime.Config{}); err != nil {
				t.Fatalf("seed runtime: %v", err)
			}
			err := newStart(t, mgr, id)()
			if !errors.Is(err, runtime.ErrSessionExists) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, want both session-exists and runtime-unavailable", entry, err)
			}
		})
	}
}

// Production puts hybrid, and auto over it, in front of the caching provider.
// Neither claims the fresh read for a leaf that may not have it, so the
// manager asks the leaf serving the name: a stale "absent" through them
// converges on a live runtime, and an unreadable one starts nothing.
func TestEnsureRunning_CompositeInFrontOfCachingProviderReadsTheLeafFresh(t *testing.T) {
	never := func(string) bool { return false }
	chains := map[string]func(*cachingFreshProvider) runtime.Provider{
		"hybrid": func(p *cachingFreshProvider) runtime.Provider {
			return hybrid.New(p, runtime.NewFake(), never)
		},
		"auto over hybrid": func(p *cachingFreshProvider) runtime.Provider {
			return auto.New(hybrid.New(p, runtime.NewFake(), never), runtime.NewFake())
		},
	}
	for chain, wrap := range chains {
		for entry, newStart := range startEntry {
			t.Run(chain+"/"+entry+"/running", func(t *testing.T) {
				mgr, sp, _, id, _ := seedUnconfirmedSessionVia(t, wrap, StateAsleep, false, false, freshRunning)
				if err := newStart(t, mgr, id)(); err != nil {
					t.Fatalf("%s: %v", entry, err)
				}
				if got := callCount(sp.Fake, "Start"); got != 0 {
					t.Errorf("Start called %d times, want 0 for a live runtime behind %s", got, chain)
				}
			})
			t.Run(chain+"/"+entry+"/unknown", func(t *testing.T) {
				mgr, sp, _, id, _ := seedUnconfirmedSessionVia(t, wrap, StateAsleep, false, false, freshUnknown)
				err := newStart(t, mgr, id)()
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("%s error = %v, want runtime unavailable", entry, err)
				}
				if got := callCount(sp.Fake, "Start"); got != 0 {
					t.Errorf("Start called %d times, want 0", got)
				}
			})
		}
	}
}

// seedAsleepSession stores an asleep, unkeyed session bead and returns its
// manager over sp, the row's id and its runtime name.
func seedAsleepSession(t *testing.T, sp runtime.Provider) (*Manager, string, string) {
	t.Helper()
	store := beads.NewMemStore()
	mgr := newTestManager(t, store, sp)
	b, err := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{
		"state": string(StateAsleep), "provider": "claude", "work_dir": "/tmp", "command": "claude --dangerously",
	}})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}
	sessName := sessionNameFor(b.ID)
	if err := store.SetMetadata(b.ID, "session_name", sessName); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}
	return mgr, b.ID, sessName
}

// isRunningReadsBeforeStart counts the IsRunning reads of name that sp took
// before its first Start.
func isRunningReadsBeforeStart(sp *runtime.Fake, name string) int {
	reads := 0
	for _, c := range sp.Calls {
		if c.Method == "Start" {
			break
		}
		if c.Method == "IsRunning" && c.Name == name {
			reads++
		}
	}
	return reads
}

// A provider without the fresh read keeps today's IsRunning reads before
// Start, and takes no other liveness read: the count is pinned, so a gate
// that added a read for such a provider fails it.
func TestEnsureRunning_ProviderWithoutFreshReadKeepsItsIsRunningReadsBeforeStart(t *testing.T) {
	sp := runtime.NewFake()
	mgr, id, sessName := seedAsleepSession(t, sp)

	if err := startEntry["Start"](t, mgr, id)(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const readsBeforeStartToday = 4
	if reads := isRunningReadsBeforeStart(sp, sessName); reads != readsBeforeStartToday {
		t.Errorf("IsRunning before Start = %d, want %d: the same reads as without the fresh-read gate", reads, readsBeforeStartToday)
	}
}

// livenessErrorProvider reads liveness per call through
// ObserveLivenessWithError, as acp and subprocess do by probing their control
// socket, and has no FreshLivenessObserver: it is the provider the fresh-read
// gate must leave alone.
type livenessErrorProvider struct {
	*runtime.Fake
	observed atomic.Int32
}

func (p *livenessErrorProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	p.observed.Add(1)
	running := p.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

// A provider that can read liveness but cannot read it fresh gets no liveness
// read from any start entry beyond the lease read Start already took, and the same IsRunning reads before
// Start as a provider with no liveness read at all (FR-02 of ga-f6y5q8n).
func TestEnsureRunning_ProviderWithLivenessReadButNoFreshReadTakesNoExtraRead(t *testing.T) {
	// Start reads its runtime once under its lease before the mint
	// (leasedLiveness), as it always has; StartRuntimeOnly takes no lease
	// read. Anything above that is a read the gate added.
	leaseReads := map[string]int32{"Start": 1, "StartRuntimeOnly": 0}
	for entry, newStart := range startEntry {
		t.Run(entry, func(t *testing.T) {
			plain := runtime.NewFake()
			plainMgr, plainID, plainName := seedAsleepSession(t, plain)
			if err := newStart(t, plainMgr, plainID)(); err != nil {
				t.Fatalf("%s over a provider with no liveness read: %v", entry, err)
			}
			want := isRunningReadsBeforeStart(plain, plainName)

			sp := &livenessErrorProvider{Fake: runtime.NewFake()}
			mgr, id, sessName := seedAsleepSession(t, sp)
			if err := newStart(t, mgr, id)(); err != nil {
				t.Fatalf("%s: %v", entry, err)
			}

			if n := sp.observed.Load(); n != leaseReads[entry] {
				t.Errorf("ObserveLivenessWithError calls = %d, want %d: only the read the start takes under its lease, none added for a provider with no fresh read", n, leaseReads[entry])
			}
			if got := isRunningReadsBeforeStart(sp.Fake, sessName); got != want {
				t.Errorf("IsRunning before Start = %d, want %d", got, want)
			}
		})
	}
}
