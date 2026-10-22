package main

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// staleKillProvider is the fixture provider with an IsRunning that answers
// cached (tmux's state cache, up to 2s old) and a fresh liveness read that
// sees the runtime as it is, failing with readErr when set; afterRead runs
// after the nth fresh read (from 1). stop, when set, runs in place of the
// inner Stop; atStop runs first.
type staleKillProvider struct {
	runtime.Provider
	cached    bool
	readErr   error
	reads     int
	afterRead func(n int)
	stop      func(name string) error
	atStop    func()
}

func (p *staleKillProvider) IsRunning(string) bool { return p.cached }

func (p *staleKillProvider) ObserveLivenessSince(name string, _ []string, _ time.Time) (runtime.Liveness, error) {
	running := p.Provider.IsRunning(name)
	p.reads++
	if p.afterRead != nil {
		defer p.afterRead(p.reads)
	}
	return runtime.Liveness{Running: running, Alive: running}, p.readErr
}

// ObserveLivenessWithError makes the fresh read an error-bearing one, so the
// kill can confirm a runtime gone by it (runtime.FreshReadable).
func (p *staleKillProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
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

// unfencedHandle is a worker handle without KillFenced.
type unfencedHandle struct{ worker.Handle }

// A handle that cannot fence the kill fails it: exit 1, and the runtime is
// not stopped unfenced.
func TestCmdSessionKill_HandleThatCannotFenceDoesNotStop(t *testing.T) {
	const sessionName = "s-gc-kill-unfenced-handle"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)
	stopped := false
	p := &staleKillProvider{cached: true}
	p.atStop = func() { stopped = true }
	fake := installStaleKillProvider(t, p)
	old := sessionKillHandle
	sessionKillHandle = func(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, id string) (worker.Handle, error) {
		h, err := old(cityPath, store, sp, cfg, id)
		return unfencedHandle{h}, err
	}
	t.Cleanup(func() { sessionKillHandle = old })
	before := mustGetBead(t, store, bead.ID)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionKill = %d, want 1; stderr=%s", code, stderr.String())
	}
	if stopped || !fake.IsRunning(sessionName) {
		t.Error("the runtime was stopped by a handle that cannot fence the kill")
	}
	if after := mustGetBead(t, store, bead.ID); !maps.Equal(after.Metadata, before.Metadata) {
		t.Errorf("row = %v, want it untouched (was %v)", after.Metadata, before.Metadata)
	}
}

// A closed row has no lifecycle left to sync: killing a closed named
// session whose runtime is gone (the "already inactive" exit) writes nothing
// onto the closed bead.
func TestCmdSessionKill_ClosedRowIsNotSyncedAsleep(t *testing.T) {
	const sessionName = "s-gc-kill-closed"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)
	p := &staleKillProvider{cached: true}
	fake := installStaleKillProvider(t, p)
	_ = fake.Stop(sessionName)
	setKillFixtureMetadata(t, store, bead.ID, map[string]string{"state": "asleep", "wake_request": "explicit"})
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("closing the session bead: %v", err)
	}
	before := mustGetBead(t, store, bead.ID)

	var stdout, stderr bytes.Buffer
	code := cmdSessionKill([]string{bead.ID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0 (already inactive); stderr=%s", code, stderr.String())
	}
	after := mustGetBead(t, store, bead.ID)
	for k := range sessionpkg.KillSleepPatch(before.ID, before.Metadata, time.Now().UTC()) {
		if after.Metadata[k] != before.Metadata[k] {
			t.Errorf("closed row %s = %q, want %q: the asleep/killed sync wrote onto a closed bead", k, after.Metadata[k], before.Metadata[k])
		}
	}
}

// "Already inactive" is the verdict the kill reached under the lease, not
// the one read before it (#7709). Each case moves the runtime between the
// two reads and fails the stop.
func TestCmdSessionKill_AlreadyInactiveIsTheUnderLeaseVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startUp  bool // the runtime comes up after the pre-lease read; else it exits
		wantCode int
	}{
		// Read gone, then a start lands: the kill is fenced, its stop fails,
		// and the live runtime is not reported "already inactive".
		{"start lands while the kill waits", true, 1},
		// Read live, then the runtime exits: the stop of a runtime already
		// gone is the "already inactive" exit of a named session.
		{"runtime exits while the kill waits", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const sessionName = "s-gc-kill-verdict"
			store, bead, _ := newKillPokeSession(t, sessionName)
			stubKillPoke(t)
			setKillFixtureMetadata(t, store, bead.ID, map[string]string{"state": "asleep"})
			p := &staleKillProvider{cached: !tc.startUp}
			fake := installStaleKillProvider(t, p)
			if tc.startUp {
				_ = fake.Stop(sessionName)
			}
			p.afterRead = func(n int) {
				if n != 1 {
					return
				}
				if tc.startUp {
					if err := fake.Start(context.Background(), sessionName, runtime.Config{Command: "claude"}); err != nil {
						t.Errorf("starting the runtime: %v", err)
					}
					return
				}
				_ = fake.Stop(sessionName)
			}
			p.stop = func(string) error { return errors.New("tmux: kill-session failed") }

			var stdout, stderr bytes.Buffer
			code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr)
			if p.reads < 2 {
				t.Fatalf("premise: %d fresh reads, want the pre-lease one and the one under the lease", p.reads)
			}
			if code != tc.wantCode {
				t.Errorf("cmdSessionKill = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			inactive := strings.Contains(stderr.String(), "was already inactive")
			if inactive != (tc.wantCode == 0) {
				t.Errorf("stderr reports already inactive = %v, want %v; stderr=%s", inactive, tc.wantCode == 0, stderr.String())
			}
		})
	}
}
