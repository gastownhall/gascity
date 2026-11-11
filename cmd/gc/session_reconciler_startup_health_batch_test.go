package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// episodeReadCountingStore counts the reads of startup-health-episode beads
// that reach the store, but only until the provider's first Start. The wake
// loop's reads all happen before the commit phase starts any session; the
// commit phase has its own episode loads (which this change leaves alone), so
// counting stops at the first Start to isolate the loop's reads.
type episodeReadCountingStore struct {
	beads.Store
	sp *runtime.Fake

	mu      sync.Mutex
	reads   int
	listErr error // returned by every episode read when set
}

func (c *episodeReadCountingStore) started() bool {
	for _, call := range c.sp.SnapshotCalls() {
		if call.Method == "Start" {
			return true
		}
	}
	return false
}

// note records one episode read and returns the injected error, if any.
func (c *episodeReadCountingStore) note() error {
	if c.started() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.listErr
}

func (c *episodeReadCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Type == sessionpkg.StartupHealthEpisodeType {
		if err := c.note(); err != nil {
			return nil, err
		}
	}
	return c.Store.List(q)
}

func (c *episodeReadCountingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if _, ok := filters[sessionpkg.StartupHealthSessionNameMetadataKey]; ok {
		if err := c.note(); err != nil {
			return nil, err
		}
	}
	return c.Store.ListByMetadata(filters, limit, opts...)
}

func (c *episodeReadCountingStore) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// downCandidatesEnv builds an env with n asleep sessions, each desired and
// not running (a "down wake candidate"), and swaps in a counting store after
// the fixtures exist.
func downCandidatesEnv(t *testing.T, n int) (*reconcilerTestEnv, *episodeReadCountingStore, []beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	var agents []config.Agent
	var sessions []beads.Bead
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("worker-%d", i)
		agents = append(agents, config.Agent{Name: name})
		env.addDesired(name, name, false)
		session := env.createSessionBead(name, name)
		env.markSessionCreating(&session)
		sessions = append(sessions, session)
	}
	env.cfg = &config.City{Agents: agents}
	counting := &episodeReadCountingStore{Store: env.store, sp: env.sp}
	env.store = counting
	return env, counting, sessions
}

// TestReconcileSessionBeads_StartupHealthEpisodesReadOncePerTick pins that the
// wake loop reads startup-health episodes once per reconcile call, not once per
// down wake candidate (ga-cat367, refs #7170).
func TestReconcileSessionBeads_StartupHealthEpisodesReadOncePerTick(t *testing.T) {
	const candidates = 4 // within defaultMaxWakesPerTick, and above the bead's N>=3
	env, counting, sessions := downCandidatesEnv(t, candidates)

	if woken := env.reconcile(sessions); woken != candidates {
		t.Fatalf("woken = %d, want %d", woken, candidates)
	}
	if got := counting.readCount(); got != 1 {
		t.Fatalf("episode reads in the wake loop = %d for %d down candidates, want exactly 1", got, candidates)
	}
}

// TestReconcileSessionBeads_NoEpisodeReadWithoutDownCandidate pins that a tick
// with nothing down pays for no episode read at all: the index is lazy.
func TestReconcileSessionBeads_NoEpisodeReadWithoutDownCandidate(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	counting := &episodeReadCountingStore{Store: env.store, sp: env.sp}
	env.store = counting

	env.reconcile([]beads.Bead{session})

	if got := counting.readCount(); got != 0 {
		t.Fatalf("episode reads with no down candidate = %d, want 0", got)
	}
}

// TestReconcileSessionBeads_StartupHealthListFailureFailsOpenOnce pins that a
// failed episode read is memoized for the tick: every candidate fails open (the
// existing arm, which logs and wakes) and the store sees one attempt.
func TestReconcileSessionBeads_StartupHealthListFailureFailsOpenOnce(t *testing.T) {
	const candidates = 3
	env, counting, sessions := downCandidatesEnv(t, candidates)
	counting.listErr = errors.New("episode store unavailable")

	if woken := env.reconcile(sessions); woken != candidates {
		t.Fatalf("woken = %d, want %d (a failed episode read fails open)", woken, candidates)
	}
	if got := counting.readCount(); got != 1 {
		t.Fatalf("episode read attempts after a failure = %d for %d candidates, want exactly 1", got, candidates)
	}
	if !strings.Contains(env.stderr.String(), "loading startup-health episode") {
		t.Fatalf("stderr = %q, want the fail-open log line for the failed episode read", env.stderr.String())
	}
}

// seedStartupHealthEpisodes writes episodes straight into store, so a test can
// hold them in the store beneath the counting wrapper without the seeding
// counting as an episode read.
func seedStartupHealthEpisodes(t *testing.T, store beads.Store, episodes ...sessionpkg.StartupHealthEpisode) {
	t.Helper()
	front := sessionpkg.NewStore(beads.SessionStore{Store: store})
	for _, ep := range episodes {
		if err := front.SaveStartupHealthEpisode(ep); err != nil {
			t.Fatalf("seed startup-health episode for %q: %v", ep.SessionName, err)
		}
	}
}

// TestReconcileSessionBeads_StartupHealthEpisodeIndexServesEachCandidateItsOwnEpisode
// (added in review of ga-cat367, acceptance 1: "a mix with and without an
// episode, one quarantined") pins that the one per-tick read answers every down
// candidate from its own episode: the candidate in an active quarantine is held
// and has its episode mirrored onto its row, while the candidates with a
// sub-threshold episode, a lapsed quarantine and no episode are all started,
// and the store is read once.
func TestReconcileSessionBeads_StartupHealthEpisodeIndexServesEachCandidateItsOwnEpisode(t *testing.T) {
	env, counting, sessions := downCandidatesEnv(t, 4)
	now := env.clk.Now()
	seedStartupHealthEpisodes(t, counting.Store,
		sessionpkg.StartupHealthEpisode{SessionName: "worker-0", ConsecutiveCount: defaultMaxWakeAttempts, Kind: sessionpkg.FailureKindOther, QuarantinedUntil: now.Add(time.Hour)},
		sessionpkg.StartupHealthEpisode{SessionName: "worker-1", ConsecutiveCount: 2, Kind: sessionpkg.FailureKindTimeout},
		sessionpkg.StartupHealthEpisode{SessionName: "worker-2", ConsecutiveCount: defaultMaxWakeAttempts, Kind: sessionpkg.FailureKindOther, QuarantinedUntil: now.Add(-time.Minute)},
	) // worker-3 has no episode

	if woken := env.reconcile(sessions); woken != 3 {
		t.Fatalf("woken = %d, want 3 (every candidate but the quarantined worker-0)", woken)
	}
	if got := counting.readCount(); got != 1 {
		t.Fatalf("episode reads in the wake loop = %d for 4 candidates, want exactly 1", got)
	}
	started := startedSessionNames(env.sp)
	for name, want := range map[string]bool{"worker-0": false, "worker-1": true, "worker-2": true, "worker-3": true} {
		if started[name] != want {
			t.Errorf("%s started = %t, want %t", name, started[name], want)
		}
	}
	held, err := counting.Get(sessions[0].ID)
	if err != nil {
		t.Fatalf("get worker-0 row: %v", err)
	}
	if got, want := held.Metadata[startupHealthActiveCountMetadataKey], strconv.Itoa(defaultMaxWakeAttempts); got != want {
		t.Errorf("worker-0 %s = %q, want %q (its own episode's count, mirrored)", startupHealthActiveCountMetadataKey, got, want)
	}
	if got, want := held.Metadata[startupHealthActiveKindMetadataKey], string(sessionpkg.FailureKindOther); got != want {
		t.Errorf("worker-0 %s = %q, want %q (its own episode's kind, mirrored)", startupHealthActiveKindMetadataKey, got, want)
	}
}

// TestReconcileSessionBeads_StartupHealthListFailureFailsOpenForAQuarantinedCandidate
// (added in review of ga-cat367, acceptance 4: "no candidate is skipped by
// quarantine") pins that a failed episode read releases even a candidate whose
// stored episode is in an active quarantine, with one read attempt for the whole
// tick, and that every candidate logs its own fail-open line.
func TestReconcileSessionBeads_StartupHealthListFailureFailsOpenForAQuarantinedCandidate(t *testing.T) {
	const candidates = 3
	env, counting, sessions := downCandidatesEnv(t, candidates)
	seedStartupHealthEpisodes(t, counting.Store,
		sessionpkg.StartupHealthEpisode{SessionName: "worker-0", ConsecutiveCount: defaultMaxWakeAttempts, Kind: sessionpkg.FailureKindOther, QuarantinedUntil: env.clk.Now().Add(time.Hour)},
	)
	counting.listErr = errors.New("episode store unavailable")

	if woken := env.reconcile(sessions); woken != candidates {
		t.Fatalf("woken = %d, want %d (a failed episode read fails open, the quarantined candidate included)", woken, candidates)
	}
	if got := counting.readCount(); got != 1 {
		t.Fatalf("episode read attempts after a failure = %d for %d candidates, want exactly 1", got, candidates)
	}
	if !startedSessionNames(env.sp)["worker-0"] {
		t.Errorf("worker-0 was not started although the episode read failed")
	}
	for i := 0; i < candidates; i++ {
		want := fmt.Sprintf("loading startup-health episode for worker-%d:", i)
		if !strings.Contains(env.stderr.String(), want) {
			t.Errorf("stderr = %q, want the fail-open line %q for every candidate", env.stderr.String(), want)
		}
	}
}
