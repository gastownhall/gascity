package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// unknownLivenessProvider is a Fake whose listing always fails and whose
// error-bearing liveness answers "unknown" for its first unknownCalls
// observations (all of them when negative), then confirmed absent.
type unknownLivenessProvider struct {
	*runtime.Fake
	mu           sync.Mutex
	observations int
	unknownCalls int
}

func (p *unknownLivenessProvider) ListRunning(string) ([]string, error) {
	return nil, errors.New("tmux list-sessions: signal: killed")
}

func (p *unknownLivenessProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observations++
	if p.unknownCalls < 0 || p.observations <= p.unknownCalls {
		return runtime.Liveness{}, errors.Join(runtime.ErrRuntimeUnavailable, errors.New("state cache stale"))
	}
	return runtime.Liveness{}, nil
}

func (p *unknownLivenessProvider) observationCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.observations
}

func TestGracefulStopAllKeepsWaitingOnUnknownLiveness(t *testing.T) {
	sp := &unknownLivenessProvider{Fake: runtime.NewFake(), unknownCalls: 1}
	if err := sp.Start(context.Background(), "alpha", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr lockedBuffer

	gracefulStopAllWithForceSignal([]string{"alpha"}, sp, time.Minute, events.Discard, nil, beads.SessionStore{}, &stdout, &stderr, func() bool { return false })

	// Pass 1 must not read the unknown first observation as an exit: it polls
	// again, sees the confirmed absence, and only then pass 2 observes once.
	if got := sp.observationCount(); got != 3 {
		t.Fatalf("liveness observations = %d, want 3 (unknown, then absent in pass 1, then pass 2)\nstdout:\n%s", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Agent 'alpha' exited gracefully") {
		t.Fatalf("stdout = %q, want a graceful exit once the absence is confirmed", stdout.String())
	}
}

func TestGracefulStopAllReportsUnknownStateWithoutStoppedEvent(t *testing.T) {
	sp := &unknownLivenessProvider{Fake: runtime.NewFake(), unknownCalls: -1}
	if err := sp.Start(context.Background(), "alpha", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	var stdout, stderr lockedBuffer

	gracefulStopAll([]string{"alpha"}, sp, 20*time.Millisecond, rec, nil, beads.SessionStore{}, &stdout, &stderr)

	if strings.Contains(stdout.String(), "exited gracefully") || !strings.Contains(stdout.String(), "Agent 'alpha' state unknown") {
		t.Fatalf("stdout = %q, want alpha reported in an unknown state, not as a graceful exit", stdout.String())
	}
	for _, ev := range rec.Events {
		if ev.Type == events.SessionStopped {
			t.Fatalf("recorded %s for a session whose state was unknown: %+v", ev.Type, ev)
		}
	}
	if sp.IsRunning("alpha") {
		t.Fatal("an unknown-state session must still be stopped")
	}
}

func TestGracefulStopAllParksCityStopSessionInUnknownState(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "alpha",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "alpha",
			"template":     "alpha",
			"state":        "active",
			"sleep_reason": string(sessionpkg.SleepReasonCityStop),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp := &unknownLivenessProvider{Fake: runtime.NewFake(), unknownCalls: -1}
	if err := sp.Start(context.Background(), "alpha", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr lockedBuffer

	gracefulStopAll([]string{"alpha"}, sp, 20*time.Millisecond, events.Discard, nil, beads.SessionStore{Store: store}, &stdout, &stderr)

	if !strings.Contains(stdout.String(), "Agent 'alpha' state unknown") {
		t.Fatalf("stdout = %q, want alpha reported in an unknown state", stdout.String())
	}
	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != string(sessionpkg.StateAsleep) {
		t.Fatalf("state = %q, want a stopped city-stop session parked %q\nstderr:\n%s", got.Metadata["state"], sessionpkg.StateAsleep, stderr.String())
	}
}
