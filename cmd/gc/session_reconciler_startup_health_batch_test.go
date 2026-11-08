package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

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
