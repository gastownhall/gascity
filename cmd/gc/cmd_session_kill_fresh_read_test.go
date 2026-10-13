package main

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// staleKillProvider is the fixture provider with an IsRunning that answers
// cached (tmux's state cache, up to 2s old) and a fresh liveness read that
// sees the runtime as it is, failing with readErr when set. stop, when set,
// runs in place of the inner Stop; atStop runs first.
type staleKillProvider struct {
	runtime.Provider
	cached  bool
	readErr error
	stop    func(name string) error
	atStop  func()
}

func (p *staleKillProvider) IsRunning(string) bool { return p.cached }

func (p *staleKillProvider) ObserveLivenessSince(name string, _ []string, _ time.Time) (runtime.Liveness, error) {
	running := p.Provider.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, p.readErr
}

func (p *staleKillProvider) Stop(name string) error {
	if p.atStop != nil {
		p.atStop()
	}
	if p.stop != nil {
		return p.stop(name)
	}
	return p.Provider.Stop(name)
}

// installStaleKillProvider puts p over the fixture provider newKillPokeSession
// installed, and returns that inner provider.
func installStaleKillProvider(t *testing.T, p *staleKillProvider) *runtime.Fake {
	t.Helper()
	oldBuild := buildSessionProviderByName
	inner, err := oldBuild(nil, "", config.SessionConfig{}, "", "")
	if err != nil {
		t.Fatalf("building fixture provider: %v", err)
	}
	fake, ok := inner.(*runtime.Fake)
	if !ok {
		t.Fatalf("fixture provider = %T, want *runtime.Fake", inner)
	}
	p.Provider = fake
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return p, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	return fake
}

// `gc session kill` decides "the runtime is already gone" on a fresh read
// under the lease, not on a cached IsRunning: a cache that reads a live
// runtime gone (a row that does not read live, a runtime that just came up)
// must not send the kill down the unfenced path.
func TestCmdSessionKill_CachedGoneStillFences(t *testing.T) {
	const sessionName = "s-gc-kill-cached-gone"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)
	setKillFixtureMetadata(t, store, bead.ID, map[string]string{"state": "asleep"})
	var atStop beads.Bead
	p := &staleKillProvider{cached: false}
	p.atStop = func() { atStop = mustGetBead(t, store, bead.ID) }
	fake := installStaleKillProvider(t, p)
	if !fake.IsRunning(sessionName) {
		t.Fatal("premise: the runtime is live")
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := atStop.Metadata["state_reason"]; got != sessionpkg.KillPendingReason {
		t.Errorf("state_reason at the stop = %q, want the kill fence: a cached IsRunning read the live runtime gone", got)
	}
	if fake.IsRunning(sessionName) {
		t.Error("the runtime survived the kill")
	}
}

// A stop that reports an error but did land is a kill that worked, judged on
// a fresh read: a cached IsRunning that still reads the runtime live must not
// roll the fence back over a dead runtime and fail the kill.
func TestCmdSessionKill_StopErrorThatLandedSucceeds(t *testing.T) {
	const sessionName = "s-gc-kill-landed"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)
	p := &staleKillProvider{cached: true}
	fake := installStaleKillProvider(t, p)
	p.stop = func(name string) error {
		_ = fake.Stop(name)
		return errors.New("tmux: kill-session raced the exit")
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0: the stop landed; stderr=%s", code, stderr.String())
	}
	if fake.IsRunning(sessionName) {
		t.Fatal("premise: the stop removed the runtime")
	}
	after := mustGetBead(t, store, bead.ID)
	if after.Metadata["state"] != string(sessionpkg.StateAsleep) || after.Metadata["sleep_reason"] != string(sessionpkg.SleepReasonKilled) || after.Metadata["state_reason"] != "" {
		t.Errorf("row after the kill = %v, want asleep/killed with the fence lifted, not rolled back over a dead runtime", after.Metadata)
	}
}

// A fresh read that fails proves nothing: a stop error over an unreadable
// runtime fails the kill and rolls the fence back, even when the stop landed.
func TestCmdSessionKill_StopErrorOverUnreadableRuntimeFails(t *testing.T) {
	const sessionName = "s-gc-kill-unreadable"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)
	p := &staleKillProvider{cached: true}
	fake := installStaleKillProvider(t, p)
	p.stop = func(name string) error {
		_ = fake.Stop(name)
		p.readErr = errors.New("tmux: server not answering")
		return errors.New("tmux: kill-session raced the exit")
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionKill = %d, want 1: liveness after the failed stop is unreadable; stderr=%s", code, stderr.String())
	}
	if got := mustGetBead(t, store, bead.ID).Metadata["state_reason"]; got == sessionpkg.KillPendingReason {
		t.Errorf("state_reason = %q, want the fence rolled back after a failed kill", got)
	}
}
