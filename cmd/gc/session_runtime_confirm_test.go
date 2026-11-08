package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// freshAnswer is what confirmProvider's fresh liveness read answers.
type freshAnswer int

const (
	// freshRunning: the runtime is alive.
	freshRunning freshAnswer = iota
	// freshAbsent: the runtime is gone.
	freshAbsent
	// freshUnavailable: the read fails with runtime.ErrRuntimeUnavailable.
	freshUnavailable
	// freshOutstanding: the read fails with worker.ErrObservationOutstanding.
	freshOutstanding
)

// confirmProvider is the one double of the runtime-confirm tests: a Fake whose
// cached read (IsRunning) is controllable apart from the runtime it holds, and
// which implements runtime.FreshLivenessObserver with a controllable answer,
// recording each call and the since it received.
type confirmProvider struct {
	*runtime.Fake
	mu            sync.Mutex
	cachedRunning bool
	answer        freshAnswer
	flipOnStop    bool
	block         chan struct{}
	freshCalls    int
	sinces        []time.Time
}

// newConfirmProvider starts each of names in the underlying Fake and returns a
// double whose cached read says not running and whose fresh read answers a.
func newConfirmProvider(t *testing.T, a freshAnswer, names ...string) *confirmProvider {
	t.Helper()
	p := &confirmProvider{Fake: runtime.NewFake(), answer: a}
	for _, n := range names {
		if err := p.Start(context.Background(), n, runtime.Config{Command: "true"}); err != nil {
			t.Fatalf("start %s: %v", n, err)
		}
	}
	return p
}

// IsRunning is the cached read.
func (p *confirmProvider) IsRunning(string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cachedRunning
}

// Stop stops the underlying runtime and, with flipOnStop, makes the fresh read
// answer absent from then on.
func (p *confirmProvider) Stop(name string) error {
	err := p.Fake.Stop(name)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.flipOnStop {
		p.answer = freshAbsent
	}
	return err
}

// ObserveLivenessSince implements runtime.FreshLivenessObserver.
func (p *confirmProvider) ObserveLivenessSince(_ string, _ []string, since time.Time) (runtime.Liveness, error) {
	p.mu.Lock()
	p.freshCalls++
	p.sinces = append(p.sinces, since)
	a, block := p.answer, p.block
	p.mu.Unlock()
	if block != nil {
		<-block
	}
	switch a {
	case freshRunning:
		return runtime.Liveness{Running: true, Alive: true}, nil
	case freshUnavailable:
		return runtime.Liveness{}, fmt.Errorf("fresh read: %w", runtime.ErrRuntimeUnavailable)
	case freshOutstanding:
		return runtime.Liveness{}, worker.ErrObservationOutstanding
	}
	return runtime.Liveness{}, nil
}

func (p *confirmProvider) fresh() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.freshCalls
}

func (p *confirmProvider) stops(name string) int {
	return p.CountCalls("Stop", name)
}

// confirmName is a session name no other test shares, so worker.ObserveBounded's
// package-level in-flight map never couples two tests.
func confirmName(t *testing.T, base string) string {
	t.Helper()
	return base + "-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
}

// staleCreatingRow creates a creating session bead named name that passes every
// reap grace filter at the returned time.
func staleCreatingRow(t *testing.T, store beads.Store, name string) (beads.Bead, time.Time) {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": name,
			"template":     "worker",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatalf("create stale creating row: %v", err)
	}
	return b, b.CreatedAt.Add(staleCreatingStateTimeout + time.Minute)
}

func beadStatus(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b.Status
}

// activeRow creates an open active session bead named name.
func activeRow(t *testing.T, store beads.Store, name string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": name,
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create active row: %v", err)
	}
	return b
}

// Site 1, items 1a/1c/1d: the stale-creating reap closes a row only on a fresh
// absent read.
func TestReapStaleSessionBeads_ConfirmsCachedNotRunningWithFreshRead(t *testing.T) {
	tests := []struct {
		name       string
		answer     freshAnswer
		wantClosed bool
	}{
		{"fresh running keeps the row", freshRunning, false},
		{"unavailable defers", freshUnavailable, false},
		{"outstanding defers", freshOutstanding, false},
		{"fresh absent closes (control)", freshAbsent, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			store := beads.NewMemStore()
			name := confirmName(t, "worker")
			sp := newConfirmProvider(t, tt.answer, name)
			b, now := staleCreatingRow(t, store, name)
			var stderr bytes.Buffer
			got := reapStaleSessionBeads(city, store, testSeatWork(city, nil, store, nil), sp, nil, nil, &clock.Fake{Time: now}, &stderr)
			closed := beadStatus(t, store, b.ID) == "closed"
			if closed != tt.wantClosed || (got == 1) != tt.wantClosed {
				t.Fatalf("closed=%v reaped=%d, want closed=%v\nstderr: %s", closed, got, tt.wantClosed, stderr.String())
			}
			if !tt.wantClosed && sp.stops(name) != 0 {
				t.Fatalf("Stop called %d times for a row that was not confirmed stopped", sp.stops(name))
			}
			if sp.fresh() != 1 {
				t.Fatalf("fresh reads = %d, want 1", sp.fresh())
			}
		})
	}
}

// Item 1b: a bead-scoped pool row's release stops the runtime unconditionally,
// so an unconfirmed read must not reach it.
func TestReapStaleSessionBeads_PoolRowRunningPerFreshReadIsNotStopped(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "placeholder",
			"template":             "claude",
			"agent_name":           "claude",
			"state":                "creating",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("create pool row: %v", err)
	}
	name := PoolSessionName("claude", b.ID)
	if err := store.SetMetadata(b.ID, "session_name", name); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	sp := newConfirmProvider(t, freshRunning, name)
	now := b.CreatedAt.Add(staleCreatingStateTimeout + time.Minute)
	var stderr bytes.Buffer
	if got := reapStaleSessionBeads(city, store, testSeatWork(city, nil, store, nil), sp, nil, nil, &clock.Fake{Time: now}, &stderr); got != 0 {
		t.Fatalf("reaped %d, want 0\nstderr: %s", got, stderr.String())
	}
	if sp.stops(name) != 0 {
		t.Fatalf("Stop called %d times on a runtime the fresh read found running", sp.stops(name))
	}
	if beadStatus(t, store, b.ID) == "closed" {
		t.Fatal("pool row closed although its runtime is running")
	}
}

// Item 1c: after the first unknown in a pass the remaining rows defer without
// a probe (the #7170 stall guard).
func TestReapStaleSessionBeads_LatchProbesOnceAfterFirstUnknown(t *testing.T) {
	for _, a := range []freshAnswer{freshUnavailable, freshOutstanding} {
		t.Run(fmt.Sprint(a), func(t *testing.T) {
			city := t.TempDir()
			store := beads.NewMemStore()
			sp := newConfirmProvider(t, a)
			var ids []string
			var now time.Time
			for i := 0; i < 3; i++ {
				b, n := staleCreatingRow(t, store, confirmName(t, fmt.Sprintf("worker-%d", i)))
				ids, now = append(ids, b.ID), n
			}
			var stderr bytes.Buffer
			if got := reapStaleSessionBeads(city, store, testSeatWork(city, nil, store, nil), sp, nil, nil, &clock.Fake{Time: now}, &stderr); got != 0 {
				t.Fatalf("reaped %d, want 0\nstderr: %s", got, stderr.String())
			}
			if sp.fresh() != 1 {
				t.Fatalf("fresh reads = %d across 3 rows, want exactly 1", sp.fresh())
			}
			for _, id := range ids {
				if beadStatus(t, store, id) == "closed" {
					t.Fatalf("row %s closed on an unanswered confirm", id)
				}
			}
			if !strings.Contains(stderr.String(), "deferring") {
				t.Fatalf("stderr lacks the deferral line:\n%s", stderr.String())
			}
		})
	}
}

// Item 1e: a cached running read never pays for a fresh one.
func TestReapStaleSessionBeads_CachedRunningNeverReadsFresh(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	name := confirmName(t, "worker")
	sp := newConfirmProvider(t, freshAbsent, name)
	sp.cachedRunning = true
	_, now := staleCreatingRow(t, store, name)
	reapStaleSessionBeads(city, store, testSeatWork(city, nil, store, nil), sp, nil, nil, &clock.Fake{Time: now}, &bytes.Buffer{})
	if sp.fresh() != 0 {
		t.Fatalf("fresh reads = %d, want 0 when the cached read says running", sp.fresh())
	}
}

// Site 2, item 2: the close-if-stopped guard for each caller's reason.
func TestCloseSeatIfRuntimeStopped_ConfirmsWithFreshRead(t *testing.T) {
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	for _, reason := range []string{"reconfigured", "suspended", "orphaned"} {
		for _, tt := range []struct {
			name       string
			cached     bool
			answer     freshAnswer
			wantClosed bool
			wantFresh  int
		}{
			{"fresh running declines", false, freshRunning, false, 1},
			{"unavailable declines", false, freshUnavailable, false, 1},
			{"outstanding declines", false, freshOutstanding, false, 1},
			{"fresh absent closes", false, freshAbsent, true, 1},
			{"cached running declines without a fresh read", true, freshAbsent, false, 0},
		} {
			t.Run(reason+"/"+tt.name, func(t *testing.T) {
				city := t.TempDir()
				store := beads.NewMemStore()
				name := confirmName(t, "worker")
				sp := newConfirmProvider(t, tt.answer, name)
				sp.cachedRunning = tt.cached
				b := activeRow(t, store, name)
				var stderr bytes.Buffer
				closed := closeSeatIfRuntimeStopped(city, store, testSeatWork(city, nil, store, nil), sp, sessionInfoFromBead(b), closeWhy{reason: reason, gate: closeGated}, now, nil, &stderr)
				if closed != tt.wantClosed {
					t.Fatalf("closed = %v, want %v\nstderr: %s", closed, tt.wantClosed, stderr.String())
				}
				if got := beadStatus(t, store, b.ID) == "closed"; got != tt.wantClosed {
					t.Fatalf("bead closed = %v, want %v", got, tt.wantClosed)
				}
				if sp.fresh() != tt.wantFresh {
					t.Fatalf("fresh reads = %d, want %d", sp.fresh(), tt.wantFresh)
				}
				if !tt.wantClosed && !strings.Contains(stderr.String(), b.ID) {
					t.Fatalf("stderr does not name %s:\n%s", b.ID, stderr.String())
				}
				if sp.stops(name) != 0 {
					t.Fatal("the close guard must never stop a runtime")
				}
			})
		}
	}
}

// Item 5: since is the wall clock, not the reconciler's injected clock.
func TestCloseSeatIfRuntimeStopped_FreshReadSinceIsWallClock(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	name := confirmName(t, "worker")
	sp := newConfirmProvider(t, freshAbsent, name)
	b := activeRow(t, store, name)
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Now()
	closeSeatIfRuntimeStopped(city, store, testSeatWork(city, nil, store, nil), sp, sessionInfoFromBead(b), closeWhy{reason: "orphaned", gate: closeGated}, past, nil, &bytes.Buffer{})
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.sinces) != 1 {
		t.Fatalf("fresh reads = %d, want 1", len(sp.sinces))
	}
	if d := sp.sinces[0].Sub(before); d < -time.Second || d > 10*time.Second {
		t.Fatalf("since = %v, want within seconds of the wall clock %v (the injected clock was %v)", sp.sinces[0], before, past)
	}
}

// stopHelpers are the two forms of the stop-before-retire helper.
var stopHelpers = map[string]func(city string, store beads.Store, sp runtime.Provider, b beads.Bead, stderr *bytes.Buffer) bool{
	"raw": func(city string, store beads.Store, sp runtime.Provider, b beads.Bead, stderr *bytes.Buffer) bool {
		return stopRuntimeBeforeSessionBeadMutation(city, store, sp, nil, b, "retired", nil, stderr)
	},
	"info": func(city string, store beads.Store, sp runtime.Provider, b beads.Bead, stderr *bytes.Buffer) bool {
		return stopRuntimeBeforeSessionBeadMutationInfo(city, store, sp, nil, sessionInfoFromBead(b), "retired", nil, stderr)
	},
}

// Site 3, item 3: both stop-before-retire helpers.
func TestStopRuntimeBeforeSessionBeadMutation_ConfirmsWithFreshRead(t *testing.T) {
	for form, stop := range stopHelpers {
		tests := []struct {
			name       string
			cached     bool
			answer     freshAnswer
			flipOnStop bool
			want       bool
			wantStops  int
		}{
			{"3a fresh running, stop takes", false, freshRunning, true, true, 1},
			{"3b fresh running before and after the stop", false, freshRunning, false, false, 1},
			{"3c unavailable defers before any kill", false, freshUnavailable, false, false, 0},
			{"3c outstanding defers before any kill", false, freshOutstanding, false, false, 0},
			{"3d fresh absent needs no stop", false, freshAbsent, false, true, 0},
		}
		for _, tt := range tests {
			t.Run(form+"/"+tt.name, func(t *testing.T) {
				city := t.TempDir()
				store := beads.NewMemStore()
				name := confirmName(t, "worker")
				sp := newConfirmProvider(t, tt.answer, name)
				sp.cachedRunning, sp.flipOnStop = tt.cached, tt.flipOnStop
				b := activeRow(t, store, name)
				var stderr bytes.Buffer
				if got := stop(city, store, sp, b, &stderr); got != tt.want {
					t.Fatalf("returned %v, want %v\nstderr: %s", got, tt.want, stderr.String())
				}
				if sp.stops(name) != tt.wantStops {
					t.Fatalf("Stop calls = %d, want %d", sp.stops(name), tt.wantStops)
				}
				if !tt.want && !strings.Contains(stderr.String(), b.ID) {
					t.Fatalf("stderr does not name %s:\n%s", b.ID, stderr.String())
				}
			})
		}
	}
}

// Item 3e: a cached running read runs the kill path as today, with no fresh
// read before it.
func TestStopRuntimeBeforeSessionBeadMutation_CachedRunningKillsAsToday(t *testing.T) {
	for form, stop := range stopHelpers {
		t.Run(form, func(t *testing.T) {
			city := t.TempDir()
			store := beads.NewMemStore()
			name := confirmName(t, "worker")
			sp := newConfirmProvider(t, freshAbsent, name)
			sp.cachedRunning = true
			b := activeRow(t, store, name)
			stop(city, store, sp, b, &bytes.Buffer{})
			if sp.stops(name) != 1 {
				t.Fatalf("Stop calls = %d, want 1", sp.stops(name))
			}
		})
	}
}

// Item 3 end to end: the removed-named-session retire neither retires nor
// releases the session's work when the runtime is not confirmed stopped.
func TestRetireRemovedConfiguredNamedSessionBead_UnconfirmedRuntimeKeepsWork(t *testing.T) {
	for _, a := range []freshAnswer{freshRunning, freshUnavailable} {
		t.Run(fmt.Sprint(a), func(t *testing.T) {
			city := t.TempDir()
			store := beads.NewMemStore()
			name := confirmName(t, "removed")
			sp := newConfirmProvider(t, a, name)
			b, err := store.Create(beads.Bead{
				Title:  "removed reviewer",
				Type:   sessionBeadType,
				Status: "open",
				Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"session_name":               name,
					"template":                   "worker",
					"state":                      "active",
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "reviewer",
					namedSessionModeMetadata:     "on_demand",
				},
			})
			if err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			work, err := store.Create(beads.Bead{Title: "owned", Type: "task", Status: "in_progress", Assignee: b.ID})
			if err != nil {
				t.Fatalf("create work: %v", err)
			}
			var stderr bytes.Buffer
			if retireRemovedConfiguredNamedSessionBead(city, store, testSeatWork(city, nil, store, nil), sp, b, time.Now().UTC(), nil, &stderr) {
				t.Fatalf("retired a session whose runtime was not confirmed stopped\nstderr: %s", stderr.String())
			}
			got, err := store.Get(work.ID)
			if err != nil {
				t.Fatalf("get work: %v", err)
			}
			if got.Assignee != b.ID || got.Status != "in_progress" {
				t.Fatalf("work = assignee %q status %q, want it left with %s", got.Assignee, got.Status, b.ID)
			}
			if after, _ := store.Get(b.ID); after.Metadata["state"] == "archived" {
				t.Fatal("session bead archived on an unconfirmed read")
			}
		})
	}
}

// Item 3 end to end: the duplicate-bead retire keeps the loser and its work
// when the loser's runtime is not confirmed stopped.
func TestRetireDuplicateConfiguredNamedSessionBeads_UnconfirmedRuntimeKeepsLoser(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", Mode: "on_demand"}},
	}
	winnerName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "reviewer")
	loserName := confirmName(t, "old-reviewer")
	sp := newConfirmProvider(t, freshUnavailable, loserName)
	mk := func(name, gen string) beads.Bead {
		b, err := store.Create(beads.Bead{
			Title:  "reviewer",
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":               name,
				"template":                   "worker",
				"generation":                 gen,
				"state":                      "active",
				namedSessionMetadataKey:      "true",
				namedSessionIdentityMetadata: "reviewer",
				namedSessionModeMetadata:     "on_demand",
			},
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return b
	}
	loser, winner := mk(loserName, "1"), mk(winnerName, "2")
	work, err := store.Create(beads.Bead{Title: "owned", Type: "task", Status: "open", Assignee: loser.ID})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	retired := retireDuplicateConfiguredNamedSessionBeads(
		city, store, testSeatWork(city, cfg, store, nil), sp, cfg, "test-city",
		[]beads.Bead{loser, winner},
		map[string]beads.Bead{loserName: loser, winnerName: winner},
		map[string]int{loserName: 0, winnerName: 1},
		time.Now().UTC(), nil, &bytes.Buffer{},
	)
	if sp.stops(loserName) != 0 {
		t.Fatalf("loser runtime stopped %d times on an unconfirmed read", sp.stops(loserName))
	}
	if retired[0].Metadata["state"] == "archived" {
		t.Fatal("loser archived on an unconfirmed read")
	}
	if got, _ := store.Get(work.ID); got.Assignee != loser.ID {
		t.Fatalf("work assignee = %q, want it left with the loser %s", got.Assignee, loser.ID)
	}
}

// Item 4: a provider without ObserveLivenessSince keeps today's behavior.
func TestRuntimeConfirm_BoolOnlyProviderUnchanged(t *testing.T) {
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	t.Run("close guard closes a stopped session", func(t *testing.T) {
		city := t.TempDir()
		store := beads.NewMemStore()
		b := activeRow(t, store, confirmName(t, "worker"))
		if !closeSeatIfRuntimeStopped(city, store, testSeatWork(city, nil, store, nil), runtime.NewFake(), sessionInfoFromBead(b), closeWhy{reason: "orphaned", gate: closeGated}, now, nil, &bytes.Buffer{}) {
			t.Fatal("a plain Fake that does not run the session must close")
		}
	})
	t.Run("close guard declines a running session", func(t *testing.T) {
		city := t.TempDir()
		store := beads.NewMemStore()
		name := confirmName(t, "worker")
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
			t.Fatal(err)
		}
		b := activeRow(t, store, name)
		if closeSeatIfRuntimeStopped(city, store, testSeatWork(city, nil, store, nil), sp, sessionInfoFromBead(b), closeWhy{reason: "orphaned", gate: closeGated}, now, nil, &bytes.Buffer{}) {
			t.Fatal("a plain Fake running the session must decline")
		}
	})
	t.Run("stop helper treats a stopped session as nothing to stop", func(t *testing.T) {
		city := t.TempDir()
		store := beads.NewMemStore()
		b := activeRow(t, store, confirmName(t, "worker"))
		for form, stop := range stopHelpers {
			if !stop(city, store, runtime.NewFake(), b, &bytes.Buffer{}) {
				t.Fatalf("%s: a plain Fake that does not run the session must return true", form)
			}
		}
	})
	t.Run("reap closes a stopped creating row", func(t *testing.T) {
		city := t.TempDir()
		store := beads.NewMemStore()
		b, at := staleCreatingRow(t, store, confirmName(t, "worker"))
		if got := reapStaleSessionBeads(city, store, testSeatWork(city, nil, store, nil), runtime.NewFake(), nil, nil, &clock.Fake{Time: at}, &bytes.Buffer{}); got != 1 {
			t.Fatalf("reaped %d, want 1", got)
		}
		if beadStatus(t, store, b.ID) != "closed" {
			t.Fatal("row not closed")
		}
	})
}

// Item 6: a fresh read that never answers defers within a small multiple of
// the bound instead of hanging.
func TestRuntimeConfirm_BlockedFreshReadDefersWithinBound(t *testing.T) {
	old := runtimeConfirmBound
	runtimeConfirmBound = 50 * time.Millisecond
	t.Cleanup(func() { runtimeConfirmBound = old })

	city := t.TempDir()
	store := beads.NewMemStore()
	name := confirmName(t, "worker")
	sp := newConfirmProvider(t, freshAbsent, name)
	sp.block = make(chan struct{})
	t.Cleanup(func() { close(sp.block) })
	b := activeRow(t, store, name)

	start := time.Now()
	closed := closeSeatIfRuntimeStopped(city, store, testSeatWork(city, nil, store, nil), sp, sessionInfoFromBead(b), closeWhy{reason: "orphaned", gate: closeGated}, time.Now(), nil, &bytes.Buffer{})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("site took %v with a 50ms bound; it hung on the blocked read", elapsed)
	}
	if closed {
		t.Fatal("closed a session whose fresh read never answered")
	}
}
