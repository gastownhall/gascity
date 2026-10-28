package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A never-started pool create that the executor defers behind a start gate
// (the per-tick wake budget or the async start limiter) has done nothing wrong
// and has not been offered a start, yet pendingCreateNeverStartedTimeout runs
// from its creation and the rollback runs before the executor in the same
// pass. startDeferralMemo carries the executor's own record of those
// deferrals into the next pass so the rollback keeps the create instead of
// closing it as a failed create and minting a replacement behind it (#7170).
//
// The memo is in-memory and process-local: it writes nothing to any bead.

// committedDeferrals returns the memo's committed set, sorted.
func committedDeferrals(m *startDeferralMemo) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.committed))
}

// seenDeferrals returns the memo's snapshot record, sorted. A memo that has
// published nothing returns nil.
func seenDeferrals(m *startDeferralMemo) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.seen))
}

// commitDeferralPass records ids as one completed pass whose snapshot held
// exactly ids and which deferred all of them.
func commitDeferralPass(m *startDeferralMemo, ids ...string) {
	commitSeenDeferralPass(m, ids, ids...)
}

// commitSeenDeferralPass records one completed pass whose snapshot held seen and
// which deferred the ids in deferred.
func commitSeenDeferralPass(m *startDeferralMemo, seen []string, deferred ...string) {
	pass := m.beginPass(neverStartedSet(seen...))
	for _, id := range deferred {
		pass.noteGateDeferred(id)
	}
	pass.finish(context.Background())
}

// neverStartedSet returns ids as the non-nil set a pass is begun with.
func neverStartedSet(ids ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// deferralMemoOptions installs m and an empty (but non-nil) planning snapshot,
// for tests that drive the executor directly rather than through a reconcile.
func deferralMemoOptions(m *startDeferralMemo) []startExecutionOption {
	return []startExecutionOption{withStartDeferralMemo(m), withStartDeferralSeen(neverStartedSet())}
}

func TestStartDeferralMemoRecordsOnlyStartGateDeferrals(t *testing.T) {
	t.Run("wake budget skips the batch tail", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		memo := newStartDeferralMemo()
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
		e.limitToOneWake()
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)

		if got := e.start(context.Background(), a, b); got != 1 {
			t.Fatalf("started %d, want 1 under a budget of 1", got)
		}
		deferred := a.info.ID
		if e.sp.IsRunning("a") {
			deferred = b.info.ID
		}
		if got, want := committedDeferrals(memo), []string{deferred}; !slices.Equal(got, want) {
			t.Fatalf("committed = %v, want exactly the budget-deferred create %v", got, want)
		}
	})

	t.Run("wake budget skips a later wave", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		memo := newStartDeferralMemo()
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
		e.limitToOneWake()
		e.cfg.Agents[1].DependsOn = []string{"a"}
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)

		if got := e.start(context.Background(), a, b); got != 1 {
			t.Fatalf("started %d, want 1 under a budget of 1", got)
		}
		if !e.sp.IsRunning("a") || e.sp.IsRunning("b") {
			t.Fatalf("premise: a running=%v b running=%v, want only the first wave started", e.sp.IsRunning("a"), e.sp.IsRunning("b"))
		}
		if got, want := committedDeferrals(memo), []string{b.info.ID}; !slices.Equal(got, want) {
			t.Fatalf("committed = %v, want the wave-level budget skip %v", got, want)
		}
	})

	t.Run("async start limiter refusal", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a")
		memo := newStartDeferralMemo()
		limiter := newAsyncStartLimiter(1)
		release, reserved, outcome := limiter.reserve(context.Background())
		if !reserved {
			t.Fatalf("premise: reserve = %q, want the only slot", outcome)
		}
		defer release()
		var tracker asyncStartTracker
		opts := append(slices.Clone(e.startOptions), deferralMemoOptions(memo)...)
		opts = append(opts, withAsyncStartExecution(), withAsyncStartLimiter(limiter), withAsyncStartTracker(&tracker))
		a := e.pendingCreate(t, "a", "", nil)

		executePlannedStartsTraced(context.Background(), []startCandidate{a}, e.cfg, e.desiredState, e.sp, e.store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
		if !tracker.wait(hangBudget) {
			t.Fatalf("async starts did not finish within the hang budget (%s)", hangBudget)
		}
		if e.sp.IsRunning("a") {
			t.Fatal("premise: a started although the limiter had no free slot")
		}
		if got, want := committedDeferrals(memo), []string{a.info.ID}; !slices.Equal(got, want) {
			t.Fatalf("committed = %v, want the limiter-deferred create %v", got, want)
		}
	})

	t.Run("a dependency block is not a start gate", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		memo := newStartDeferralMemo()
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
		e.cfg.Agents[1].DependsOn = []string{"a"}
		e.sp.StartErrors["a"] = errors.New("provider start failure")
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)

		e.start(context.Background(), a, b)
		if e.sp.IsRunning("b") {
			t.Fatal("premise: b started although its dependency failed")
		}
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v, want nothing: blocked_on_dependencies and a failed start are not start-gate deferrals", got)
		}
	})

	t.Run("an endpoint capacity refusal is not a start gate", func(t *testing.T) {
		e := newCapacityEnv(t, true, "a")
		memo := newStartDeferralMemo()
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
		a := e.pendingCreate(t, "a", "e", nil)
		e.openEndpoint(t)

		e.start(context.Background(), a)
		if e.sp.IsRunning("a") {
			t.Fatal("premise: a started although its endpoint is refusing")
		}
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v, want nothing: the endpoint hold already keeps this row", got)
		}
	})

	t.Run("a started create is not recorded", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a")
		memo := newStartDeferralMemo()
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
		a := e.pendingCreate(t, "a", "", nil)

		if got := e.start(context.Background(), a); got != 1 {
			t.Fatalf("started %d, want 1", got)
		}
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v, want nothing for a start that went through", got)
		}
	})
}

func TestStartDeferralMemoCommitReplacesAndCanceledRunCommitsNothing(t *testing.T) {
	t.Run("a pass that defers nothing empties the set and publishes its snapshot", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a")
		memo := newStartDeferralMemo()
		commitDeferralPass(memo, "stale-1", "stale-2")
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
		a := e.pendingCreate(t, "a", "", nil)

		e.start(context.Background(), a)
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v, want the prior set replaced, not merged", got)
		}
		if got := seenDeferrals(memo); len(got) != 0 {
			t.Fatalf("seen = %v, want this pass's snapshot replacing the prior one", got)
		}
	})

	t.Run("a run with no candidates still commits", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a")
		memo := newStartDeferralMemo()
		commitDeferralPass(memo, "stale-1")
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)

		e.start(context.Background())
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v, want the empty pass to empty the set", got)
		}
		if got := seenDeferrals(memo); len(got) != 0 {
			t.Fatalf("seen = %v, want the empty pass to publish its empty snapshot: an empty pass is still a pass", got)
		}
	})

	t.Run("a run canceled at entry leaves the committed record alone", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a")
		memo := newStartDeferralMemo()
		commitDeferralPass(memo, "stale-1")
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
		a := e.pendingCreate(t, "a", "", nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		e.start(ctx, a)
		if got, want := committedDeferrals(memo), []string{"stale-1"}; !slices.Equal(got, want) {
			t.Fatalf("committed = %v, want %v untouched", got, want)
		}
		if got, want := seenDeferrals(memo), []string{"stale-1"}; !slices.Equal(got, want) {
			t.Fatalf("seen = %v, want %v untouched", got, want)
		}
	})

	t.Run("a run canceled after recording a deferral commits nothing and leaks nothing", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		memo := newStartDeferralMemo()
		commitDeferralPass(memo, "stale-1")
		e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
		e.limitToOneWake()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		provider := &cancelOnStartProvider{Fake: e.sp, cancel: cancel}
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)

		executePlannedStartsTraced(ctx, []startCandidate{a, b}, e.cfg, e.desiredState, provider, e.store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, e.startOptions...)
		if ctx.Err() == nil {
			t.Fatal("premise: the run was never canceled")
		}
		if got, want := committedDeferrals(memo), []string{"stale-1"}; !slices.Equal(got, want) {
			t.Fatalf("committed = %v, want %v: a canceled run commits nothing", got, want)
		}
		if got, want := seenDeferrals(memo), []string{"stale-1"}; !slices.Equal(got, want) {
			t.Fatalf("seen = %v, want %v untouched", got, want)
		}

		// The canceled run's deferral must not surface in a later pass.
		e.clk.Advance(time.Minute)
		e.start(context.Background())
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v after a clean empty pass, want nothing: the canceled run leaked its deferral", got)
		}
	})
}

// cancelOnStartProvider cancels the run's context while the first Start runs.
type cancelOnStartProvider struct {
	*runtime.Fake
	cancel context.CancelFunc
}

func (p *cancelOnStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.cancel()
	return p.Fake.Start(ctx, name, cfg)
}

// deferralInfo builds a never-started pending-create row.
func deferralInfo(created, startedAt time.Time) sessionpkg.Info {
	info := sessionpkg.Info{ID: "x", PendingCreateClaim: true, CreatedAt: created}
	if !startedAt.IsZero() {
		info.PendingCreateStartedAt = startedAt.UTC().Format(time.RFC3339)
	}
	return info
}

func TestStartDeferralMemoHolds(t *testing.T) {
	created := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	before := created.Add(-5 * time.Minute)

	for _, tc := range []struct {
		name      string
		committed []string // ids the last completed pass deferred at a start gate
		seen      []string // never-started ids that pass's snapshot held
		noPass    bool     // no pass has completed yet
		info      sessionpkg.Info
		now       time.Time
		want      bool
	}{
		{
			name:      "committed and past the lease",
			committed: []string{"x"}, seen: []string{"x"},
			info: deferralInfo(before, before), now: before.Add(11 * time.Minute), want: true,
		},
		{
			name:      "committed, just inside the ceiling",
			committed: []string{"x"}, seen: []string{"x"},
			info: deferralInfo(before, before), now: before.Add(startDeferralHoldCeiling - time.Second), want: true,
		},
		{
			name:      "committed, past the ceiling",
			committed: []string{"x"}, seen: []string{"x"},
			info: deferralInfo(before, before), now: before.Add(startDeferralHoldCeiling + time.Second), want: false,
		},
		{
			name:      "committed, exactly at the ceiling: the hold ends only after it",
			committed: []string{"x"}, seen: []string{"x"},
			info: deferralInfo(before, before), now: before.Add(startDeferralHoldCeiling), want: true,
		},
		{
			name:      "seen but not committed: it was offered a start",
			committed: []string{"other"}, seen: []string{"x", "other"},
			info: deferralInfo(before, before), now: before.Add(11 * time.Minute), want: false,
		},
		{
			name:      "a pass that deferred nothing still ends the first-chance window of what it saw",
			committed: nil, seen: []string{"x"},
			info: deferralInfo(before, before), now: before.Add(11 * time.Minute), want: false,
		},
		{
			name:      "not in the last pass's snapshot: first chance",
			committed: []string{"other"}, seen: []string{"other"},
			info: deferralInfo(created, created), now: created.Add(11 * time.Minute), want: true,
		},
		{
			name:      "an empty snapshot is still an observation: an unseen row gets its first chance",
			committed: nil, seen: nil,
			info: deferralInfo(before, before), now: before.Add(11 * time.Minute), want: true,
		},
		{
			name:      "first chance regardless of how old the row is next to the pass",
			committed: nil, seen: []string{"other"},
			info: deferralInfo(before, before), now: before.Add(11 * time.Minute), want: true,
		},
		{
			name:      "first chance still honors the ceiling",
			committed: []string{"other"}, seen: []string{"other"},
			info: deferralInfo(created, created), now: created.Add(startDeferralHoldCeiling + time.Second), want: false,
		},
		{
			name:      "first chance in a pass that outlasts the lease",
			committed: nil, seen: []string{"other"},
			info: deferralInfo(created, created), now: created.Add(30 * time.Minute), want: true,
		},
		{
			name:   "no pass yet: one pass of grace for every row",
			noPass: true,
			info:   deferralInfo(before, before), now: before.Add(11 * time.Minute), want: true,
		},
		{
			name:      "a row that is not a claimed pending create is never held",
			committed: []string{"x"}, seen: nil,
			info: func() sessionpkg.Info {
				i := deferralInfo(before, before)
				i.PendingCreateClaim = false
				return i
			}(),
			now: before.Add(11 * time.Minute), want: false,
		},
		{
			name:      "a started row is never held",
			committed: []string{"x"}, seen: nil,
			info: func() sessionpkg.Info {
				i := deferralInfo(before, before)
				i.LastWokeAt = before.Add(time.Minute).UTC().Format(time.RFC3339)
				return i
			}(),
			now: before.Add(11 * time.Minute), want: false,
		},
		{
			name:      "a row with no anchor is never held",
			committed: []string{"x"}, seen: nil,
			info: deferralInfo(time.Time{}, time.Time{}), now: created, want: false,
		},
		{
			name:      "an unparseable claim time falls back to creation for the ceiling",
			committed: []string{"x"}, seen: []string{"x"},
			info: func() sessionpkg.Info {
				i := deferralInfo(before, time.Time{})
				i.PendingCreateStartedAt = "not-a-time"
				return i
			}(),
			now: before.Add(startDeferralHoldCeiling + time.Second), want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memo := newStartDeferralMemo()
			if !tc.noPass {
				commitSeenDeferralPass(memo, tc.seen, tc.committed...)
			}
			if got := memo.Holds(tc.info, tc.now); got != tc.want {
				t.Fatalf("Holds = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("an empty session id is never recorded", func(t *testing.T) {
		memo := newStartDeferralMemo()
		pass := memo.beginPass(neverStartedSet())
		pass.noteGateDeferred("")
		pass.finish(context.Background())
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v, want none", got)
		}
	})

	t.Run("a nil memo holds nothing", func(t *testing.T) {
		var memo *startDeferralMemo
		if memo.Holds(deferralInfo(before, before), before.Add(11*time.Minute)) {
			t.Fatal("nil memo held a row")
		}
		memo.beginPass(neverStartedSet()).noteGateDeferred("x") // must not panic
		memo.beginPass(neverStartedSet()).finish(context.Background())
	})
}

func TestPendingCreateLeaseAnchor(t *testing.T) {
	created := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	claimed := created.Add(3 * time.Minute)
	for _, tc := range []struct {
		name        string
		info        sessionpkg.Info
		wantAnchor  time.Time
		wantExpired bool // at created+12m: past the lease from 'created' (10m), inside it from 'claimed' (13m)
	}{
		{"claim time wins when it parses", deferralInfo(created, claimed), claimed, false},
		{"claim time absent: creation", deferralInfo(created, time.Time{}), created, true},
		{"claim time unparseable: creation", func() sessionpkg.Info {
			i := deferralInfo(created, time.Time{})
			i.PendingCreateStartedAt = "garbage"
			return i
		}(), created, true},
		{"claim time only", deferralInfo(time.Time{}, claimed), claimed, false},
		{"no anchor at all: expired by definition", deferralInfo(time.Time{}, time.Time{}), time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pendingCreateLeaseAnchor(tc.info); !got.Equal(tc.wantAnchor) {
				t.Fatalf("anchor = %v, want %v", got, tc.wantAnchor)
			}
			now := created.Add(12 * time.Minute)
			if got := pendingCreateNeverStartedLeaseExpiredAt(tc.info, now); got != tc.wantExpired {
				t.Fatalf("lease expired at %v = %v, want %v", now, got, tc.wantExpired)
			}
		})
	}
}

// deferralHarness is the chaos harness set up for budget-limited pool creates:
// n desired, never-started creates of one template under max_wakes_per_tick=1.
type deferralHarness struct {
	*sessionChaosHarness
	memo  *startDeferralMemo // nil when the run models today's behavior
	ids   []string
	names []string
}

func newDeferralHarness(t *testing.T, seed int64, n int, withMemo bool) *deferralHarness {
	t.Helper()
	h := newSessionChaosHarness(t, seed)
	// The store stamps CreatedAt from wall time, so run the fake clock on wall
	// time too; the first pass then begins after every row was created.
	h.env.clk.Time = time.Now().UTC()
	h.env.cfg.Daemon.MaxWakesPerTick = intPtr(1)
	d := &deferralHarness{sessionChaosHarness: h}
	if withMemo {
		d.memo = newStartDeferralMemo()
		h.env.startOptions = append(h.env.startOptions, withStartDeferralMemo(d.memo))
	}
	for i := 0; i < n; i++ {
		d.addIntent(t)
	}
	h.env.clk.Advance(time.Minute)
	return d
}

// addIntent creates one more desired, never-started create of the template.
func (d *deferralHarness) addIntent(t *testing.T) string {
	t.Helper()
	info, err := d.manager.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: d.template, Title: "Pool worker", Command: d.command, Provider: "fake"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	tp := d.templateParams()
	tp.SessionName = info.SessionName
	d.env.desiredState[info.SessionName] = tp
	d.ids = append(d.ids, info.ID)
	d.names = append(d.names, info.SessionName)
	return info.ID
}

// tick runs one reconcile pass over every open session bead.
func (d *deferralHarness) tick(t *testing.T) int {
	t.Helper()
	sessions, err := loadSessionBeads(d.env.store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	return d.env.reconcile(sessions)
}

func (d *deferralHarness) bead(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := d.env.store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return b
}

// notRunning returns the ids whose runtime is not up.
func (d *deferralHarness) notRunning() []string {
	var out []string
	for i, name := range d.names {
		if !d.env.sp.IsRunning(name) {
			out = append(out, d.ids[i])
		}
	}
	return out
}

func (d *deferralHarness) requireNoneClosed(t *testing.T, when string) {
	t.Helper()
	for _, id := range d.ids {
		if got := d.bead(t, id); got.Status == "closed" {
			t.Fatalf("%s: create %s was closed (state=%q, reason=%q); a create waiting on a start gate must keep its bead", when, id, got.Metadata["state"], got.Metadata["close_reason"])
		}
	}
}

func TestStartGateDeferredPendingCreateSurvivesTheLeaseAndDrainsOnePerPass(t *testing.T) {
	d := newDeferralHarness(t, 20261014, 3, true)

	if got := d.tick(t); got != 1 {
		t.Fatalf("pass 1 started %d, want 1 under a budget of 1", got)
	}
	waiting := d.notRunning()
	if len(waiting) != 2 {
		t.Fatalf("premise: %d creates waiting after pass 1, want 2", len(waiting))
	}
	// Both waiting creates are now older than the 10-minute lease, and they
	// have never been offered a start that the budget did not take.
	d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	if got := d.tick(t); got != 1 {
		t.Fatalf("pass 2 started %d, want 1 under a budget of 1", got)
	}
	d.requireNoneClosed(t, "pass 2")
	left := d.notRunning()
	if len(left) != 1 {
		t.Fatalf("%d creates still waiting after pass 2, want 1", len(left))
	}
	held := d.bead(t, left[0])
	if held.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("held create lost its claim: pending_create_claim=%q state=%q (the same-tick heal undid the hold)", held.Metadata["pending_create_claim"], held.Metadata["state"])
	}
	if !strings.Contains(d.env.stderr.String(), "keeping pending create") {
		t.Fatalf("no \"keeping pending create\" log for the held create; stderr:\n%s", d.env.stderr.String())
	}

	d.env.clk.Advance(time.Minute)
	if got := d.tick(t); got != 1 {
		t.Fatalf("pass 3 started %d, want 1", got)
	}
	if got := d.notRunning(); len(got) != 0 {
		t.Fatalf("%v still waiting after pass 3, want every create started", got)
	}
	d.requireNoneClosed(t, "pass 3")
	all, err := d.env.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("%d beads exist, want 3: a held create must never be replaced by a fresh one", len(all))
	}
}

// A hold and a deferral write nothing to any bead (FR-7).
func TestStartGateDeferredPendingCreateWritesNothingWhileHeld(t *testing.T) {
	d := newDeferralHarness(t, 20261015, 3, true)
	d.tick(t) // pass 1: one starts, two wait
	snapshot := make(map[string]beads.Bead)
	for _, id := range d.notRunning() {
		snapshot[id] = d.bead(t, id)
	}
	d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	d.tick(t) // pass 2: one more starts, the other is held
	left := d.notRunning()
	if len(left) != 1 {
		t.Fatalf("premise: %d creates waiting after pass 2, want 1", len(left))
	}
	if got, want := d.bead(t, left[0]), snapshot[left[0]]; !reflect.DeepEqual(got, want) {
		t.Fatalf("a held, deferred create was written to:\nbefore: %+v\nafter:  %+v", want, got)
	}
}

func TestWithoutTheMemoAStartGateDeferredCreateIsRolledBack(t *testing.T) {
	d := newDeferralHarness(t, 20261016, 2, false)
	d.tick(t)
	waiting := d.notRunning()
	if len(waiting) != 1 {
		t.Fatalf("premise: %d creates waiting after pass 1, want 1", len(waiting))
	}
	d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	d.tick(t)
	got := d.bead(t, waiting[0])
	if got.Status != "closed" {
		t.Fatalf("with no memo the lease-expired create has status %q, want closed: this pins today's behavior", got.Status)
	}
}

func TestStartGateDeferredHoldHasACeiling(t *testing.T) {
	d := newDeferralHarness(t, 20261017, 2, true)
	d.tick(t)
	waiting := d.notRunning()
	if len(waiting) != 1 {
		t.Fatalf("premise: %d creates waiting after pass 1, want 1", len(waiting))
	}
	d.env.clk.Advance(startDeferralHoldCeiling + 2*time.Minute)
	d.tick(t)
	if got := d.bead(t, waiting[0]); got.Status != "closed" {
		t.Fatalf("a create deferred for more than %s has status %q, want closed: the hold is bounded", startDeferralHoldCeiling, got.Status)
	}
}

func TestStartGateDeferredFirstChanceHoldsARowBornAfterTheLastPass(t *testing.T) {
	t.Run("with the memo", func(t *testing.T) {
		d := newDeferralHarness(t, 20261018, 1, true)
		d.tick(t) // pass 1 starts the only create and stamps the pass start
		d.env.clk.Advance(time.Second)
		born := d.addIntent(t)
		// A slow gap before the next pass: the new row's lease runs out
		// before the executor has ever seen it.
		d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
		d.tick(t)
		if got := d.bead(t, born); got.Status == "closed" {
			t.Fatalf("a create born after the last pass began was rolled back before its first offer (state=%q)", got.Metadata["state"])
		}
		if !d.env.sp.IsRunning(d.names[len(d.names)-1]) {
			t.Fatal("the first-chance create was kept but not started")
		}
	})

	t.Run("without the memo it is rolled back", func(t *testing.T) {
		d := newDeferralHarness(t, 20261019, 1, false)
		d.tick(t)
		d.env.clk.Advance(time.Second)
		born := d.addIntent(t)
		d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
		d.tick(t)
		if got := d.bead(t, born); got.Status != "closed" {
			t.Fatalf("with no memo the lease-expired create has status %q, want closed: this pins today's behavior", got.Status)
		}
	})
}

func TestStartGateDeferredHoldReleasesWhenTheSessionIsNoLongerDesired(t *testing.T) {
	d := newDeferralHarness(t, 20261020, 2, true)
	d.tick(t)
	waiting := d.notRunning()
	if len(waiting) != 1 {
		t.Fatalf("premise: %d creates waiting after pass 1, want 1", len(waiting))
	}
	for i, id := range d.ids {
		if id == waiting[0] {
			delete(d.env.desiredState, d.names[i])
		}
	}
	d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	d.tick(t)
	if got := d.bead(t, waiting[0]); got.Status != "closed" {
		t.Fatalf("an undesired create held by the memo has status %q, want closed: the hold covers desired creates only", got.Status)
	}
}

func TestReapStaleSessionBeadsHonorsTheStartDeferralHold(t *testing.T) {
	testCity := t.TempDir()
	e := newCapacityEnv(t, false, "held", "unheld", "started", "endpoint")
	// The reaper ages rows from their store CreatedAt, which is wall time.
	e.clk.Time = time.Now().UTC()
	claimed := e.clk.Now().UTC().Format(time.RFC3339)
	creating := func(extra map[string]string) map[string]string {
		meta := map[string]string{"state": "creating", "pending_create_started_at": claimed}
		maps.Copy(meta, extra)
		return meta
	}
	held := e.pendingCreate(t, "held", "", creating(nil))
	unheld := e.pendingCreate(t, "unheld", "", creating(nil))
	started := e.pendingCreate(t, "started", "", creating(map[string]string{"last_woke_at": claimed}))
	endpoint := e.pendingCreate(t, "endpoint", "", creating(nil))

	memo := newStartDeferralMemo()
	// The last pass's snapshot held every never-started row, so none has a first
	// chance left; only the deferred ids are held.
	commitSeenDeferralPass(memo, []string{held.info.ID, unheld.info.ID, endpoint.info.ID}, held.info.ID, started.info.ID)
	e.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	hold := pendingCreateHold(func(info sessionpkg.Info) bool { return info.ID == endpoint.info.ID }, memo, e.clk)

	if n := reapStaleSessionBeads(testCity, e.store, testSeatWork(testCity, nil, e.store, nil), e.sp, e.dt, hold, e.clk, &e.log); n != 2 {
		t.Fatalf("reaped %d rows, want 2 (the unheld never-started row and the started row)\nlog: %s", n, e.log.String())
	}
	for _, tc := range []struct {
		name       string
		id         string
		wantStatus string
	}{
		{"memo-held never-started row", held.info.ID, "open"},
		{"endpoint-held row", endpoint.info.ID, "open"},
		{"unheld never-started row", unheld.info.ID, "closed"},
		{"started row, even though its id is in the memo", started.info.ID, "closed"},
	} {
		if got := e.bead(t, tc.id); got.Status != tc.wantStatus {
			t.Errorf("%s: status = %q, want %q", tc.name, got.Status, tc.wantStatus)
		}
	}

	t.Run("a nil memo and no endpoint hold hold nothing", func(t *testing.T) {
		if pendingCreateHold(nil, nil, e.clk)(sessionpkg.Info{ID: held.info.ID}) {
			t.Fatal("held a row with neither an endpoint hold nor a memo")
		}
	})
}

func TestStartGateDeferredTraceReasonIsDistinct(t *testing.T) {
	if TraceReasonStartGateDeferred != "start_gate_deferred" {
		t.Fatalf("TraceReasonStartGateDeferred = %q", TraceReasonStartGateDeferred)
	}
	if TraceReasonStartGateDeferred == TraceReasonEndpointCapacityOpen {
		t.Fatal("the start-gate hold must not reuse the endpoint-capacity reason")
	}
}

// The hold writes one decision record naming the start gate and how long the
// lease has been running, measured from the lease anchor (the claim time), not
// from the bead's creation.
func TestStartGateDeferredHoldTraceDecision(t *testing.T) {
	e := newCapacityEnv(t, false, "a", "b")
	e.limitToOneWake()
	memo := newStartDeferralMemo()
	e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
	// The store stamps CreatedAt from wall time, so run the fake clock on wall
	// time too, and claim the creates a known five minutes before pass 1.
	e.clk.Time = time.Now().UTC()
	claimed := e.clk.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	a := e.pendingCreate(t, "a", "", map[string]string{"pending_create_started_at": claimed})
	b := e.pendingCreate(t, "b", "", map[string]string{"pending_create_started_at": claimed})
	e.clk.Advance(time.Minute)

	rows := func() []beads.Bead { return []beads.Bead{e.bead(t, a.info.ID), e.bead(t, b.info.ID)} }
	if woken := e.reconcileTraced(rows()...); woken != 1 {
		t.Fatalf("pass 1 woke %d, want 1 under a budget of 1", woken)
	}
	var waiting startCandidate
	for _, c := range []startCandidate{a, b} {
		if !e.sp.IsRunning(c.tp.SessionName) {
			waiting = c
		}
	}
	if waiting.tp.SessionName == "" {
		t.Fatal("premise: no create is waiting after pass 1")
	}
	e.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	e.reconcileTraced(rows()...)

	var held []SessionReconcilerTraceRecord
	for _, r := range e.records(TraceSiteReconcilerPendingCreate) {
		if r.ReasonCode == TraceReasonStartGateDeferred {
			held = append(held, r)
		}
	}
	if len(held) != 1 {
		t.Fatalf("%d start-gate hold records, want 1", len(held))
	}
	r := held[0]
	if r.OutcomeCode != TraceOutcomeDeferred {
		t.Errorf("outcome = %q, want %q", r.OutcomeCode, TraceOutcomeDeferred)
	}
	if r.Template != waiting.tp.TemplateName {
		t.Errorf("template = %q, want %q", r.Template, waiting.tp.TemplateName)
	}
	if got := r.Fields["hold"]; got != "start_gate" {
		t.Errorf("fields[hold] = %v, want start_gate", got)
	}
	// Pass 2 reads the clock 12 minutes after the creates were made and 17
	// after the claim time they were stamped with.
	if got, want := r.Fields["anchor_age"], "17m0s"; got != want {
		t.Errorf("fields[anchor_age] = %v, want %s (measured from the claim time)", got, want)
	}
}

// A pass over a partial store read did not see every pending create, so it
// must not replace the memo's record; a full pass does.
func TestStartGateDeferredPartialStorePassLeavesTheMemoAlone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		partial     bool
		wantUntouch bool
	}{
		{"a partial-store pass leaves the record alone", true, true},
		{"a full pass replaces the record", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, false, "a", "b")
			e.limitToOneWake()
			memo := newStartDeferralMemo()
			e.startOptions = append(e.startOptions, deferralMemoOptions(memo)...)
			commitDeferralPass(memo, "held-id")
			a := e.pendingCreate(t, "a", "", nil)
			b := e.pendingCreate(t, "b", "", nil)

			poolDesired := map[string]int{"a": 1, "b": 1}
			cfgNames := configuredSessionNames(e.cfg, "", e.store)
			reconcileSessionBeadsTraced(
				context.Background(), e.city, []beads.Bead{e.bead(t, a.info.ID), e.bead(t, b.info.ID)}, e.desiredState, cfgNames, e.cfg, e.sp,
				e.store, nil, nil, nil, nil, e.dt, poolDesired, tc.partial, nil, "",
				nil, e.clk, e.rec, 0, 0, &e.log, &e.log, e.trace,
				e.startOptions...,
			)

			got := committedDeferrals(memo)
			gotSeen := seenDeferrals(memo)
			if tc.wantUntouch {
				if !slices.Equal(got, []string{"held-id"}) {
					t.Fatalf("committed = %v, want [held-id]: a partial-store pass replaced the record", got)
				}
				if !slices.Equal(gotSeen, []string{"held-id"}) {
					t.Fatalf("seen = %v, want [held-id]: a partial-store pass replaced the snapshot record", gotSeen)
				}
				return
			}
			if slices.Contains(got, "held-id") {
				t.Fatalf("committed = %v: a full pass did not replace the prior record", got)
			}
			if want := slices.Sorted(slices.Values([]string{a.info.ID, b.info.ID})); !slices.Equal(gotSeen, want) {
				t.Fatalf("seen = %v, want this pass's never-started creates %v", gotSeen, want)
			}
		})
	}
}

// A create made after a tick read its session snapshot, but before that tick's
// start executor began, is never offered in that tick (it is not in the
// snapshot). The next tick's lease check must still hold it: it has not been
// offered a start. Regression test adapted from the reproduction posted on
// PR #8298 (the first-chance boundary keys off executor entry, not the
// snapshot read). The control is the same create made after the executor began.
func TestStartDeferralMemoHoldsACreateBornBetweenSnapshotAndExecutor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bornInGap bool
	}{
		{"born between the snapshot read and executor entry", true},
		{"control: born after executor entry", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeferralHarness(t, 20261027, 1, true)
			d.tick(t) // pass 1 starts the only create
			d.env.clk.Advance(time.Minute)

			// Tick 2: the snapshot is read first.
			sessions, err := loadSessionBeads(d.env.store)
			if err != nil {
				t.Fatalf("loadSessionBeads: %v", err)
			}
			var bornID, bornName string
			create := func() {
				info, err := d.manager.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: d.template, Title: "Manual worker", Command: d.command, Provider: "fake"})
				if err != nil {
					t.Fatalf("CreateSession: %v", err)
				}
				bornID, bornName = info.ID, info.SessionName
			}
			if tc.bornInGap {
				create()
			}
			// Snapshot reload to executor entry: the bead-reconcile work
			// before the start stage.
			d.env.clk.Advance(2 * time.Minute)
			d.env.reconcile(sessions)
			if !tc.bornInGap {
				d.env.clk.Advance(time.Second)
				create()
			}
			// Tick 3 sees the create in its snapshot and desired state, after
			// a tick gap longer than the never-started lease.
			tp := d.templateParams()
			tp.SessionName = bornName
			d.env.desiredState[bornName] = tp
			d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
			d.tick(t)

			got := d.bead(t, bornID)
			t.Logf("create %s: status=%q state=%q last_woke_at=%q running=%v", bornID, got.Status, got.Metadata["state"], got.Metadata["last_woke_at"], d.env.sp.IsRunning(bornName))
			if got.Status == "closed" {
				t.Errorf("create %s rolled back (state=%q) without ever being offered a start", bornID, got.Metadata["state"])
			}
		})
	}
}

// A create the last pass's planning snapshot lacked was never offered a start
// there, even when it already existed: the snapshot read can lag the store (a
// cache that has not yet seen the write). The hold keys on what the pass saw,
// not on when the create was born.
func TestStartDeferralMemoHoldsACreateTheSnapshotMissed(t *testing.T) {
	d := newDeferralHarness(t, 20261028, 1, true)
	d.tick(t) // pass 1 starts the only create
	d.env.clk.Advance(time.Minute)

	// The create exists in the store before tick 2 reads its snapshot, but the
	// snapshot omits it: a lagging read.
	info, err := d.manager.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: d.template, Title: "Manual worker", Command: d.command, Provider: "fake"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sessions, err := loadSessionBeads(d.env.store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	lagged := slices.DeleteFunc(slices.Clone(sessions), func(b beads.Bead) bool { return b.ID == info.ID })
	if len(lagged) != len(sessions)-1 {
		t.Fatalf("premise: the lagged snapshot dropped %d rows, want exactly the new create", len(sessions)-len(lagged))
	}
	d.env.clk.Advance(time.Minute)
	d.env.reconcile(lagged)
	if slices.Contains(seenDeferrals(d.memo), info.ID) {
		t.Fatalf("premise: the lagged pass saw %s", info.ID)
	}

	// Tick 3 reads the create and the pool wants it, after a gap longer than
	// the never-started lease.
	tp := d.templateParams()
	tp.SessionName = info.SessionName
	d.env.desiredState[info.SessionName] = tp
	d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	d.tick(t)

	got := d.bead(t, info.ID)
	if got.Status == "closed" {
		t.Fatalf("create %s rolled back (state=%q) without ever being offered a start", info.ID, got.Metadata["state"])
	}
	if !d.env.sp.IsRunning(info.SessionName) {
		t.Fatal("the held create was kept but not started")
	}
}

// A create the last pass saw but did not offer a start (nothing wanted it) was
// not deferred at a start gate, so the memo has no reason to keep it past its
// lease: it rolls back as before.
func TestStartDeferralMemoDoesNotHoldACreateTheLastPassSawButDidNotOffer(t *testing.T) {
	d := newDeferralHarness(t, 20261029, 1, true)
	d.tick(t) // pass 1 starts the only create
	d.env.clk.Advance(time.Minute)

	info, err := d.manager.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: d.template, Title: "Manual worker", Command: d.command, Provider: "fake"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Tick 2 reads the create but the pool does not want it yet: not desired,
	// so the executor is never given it.
	d.env.clk.Advance(time.Minute)
	d.tick(t)
	if !slices.Contains(seenDeferrals(d.memo), info.ID) {
		t.Fatalf("premise: tick 2 did not see %s; seen = %v", info.ID, seenDeferrals(d.memo))
	}
	if slices.Contains(committedDeferrals(d.memo), info.ID) {
		t.Fatalf("premise: tick 2 deferred %s at a start gate", info.ID)
	}
	if d.env.sp.IsRunning(info.SessionName) {
		t.Fatal("premise: the create started although nothing wanted it")
	}

	tp := d.templateParams()
	tp.SessionName = info.SessionName
	d.env.desiredState[info.SessionName] = tp
	d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	d.tick(t)

	got := d.bead(t, info.ID)
	if got.Status != "closed" || got.Metadata["state"] != "failed-create" {
		t.Fatalf("create %s: status=%q state=%q, want closed as failed-create: the last pass saw it, so it already had its chance", info.ID, got.Status, got.Metadata["state"])
	}
}

// The memo's snapshot record after a reconcile run is exactly the never-started
// pending creates the run's snapshot held: a started session and a create from
// an earlier pass that has since started are not in it.
func TestStartDeferralMemoSeenIsTheNeverStartedCreatesOfTheSnapshot(t *testing.T) {
	d := newDeferralHarness(t, 20261030, 3, true)
	if got := d.tick(t); got != 1 {
		t.Fatalf("pass 1 started %d, want 1 under a budget of 1", got)
	}
	if got, want := seenDeferrals(d.memo), slices.Sorted(slices.Values(d.ids)); !slices.Equal(got, want) {
		t.Fatalf("seen after pass 1 = %v, want all three never-started creates %v", got, want)
	}

	waiting := d.notRunning()
	if len(waiting) != 2 {
		t.Fatalf("premise: %d creates waiting after pass 1, want 2", len(waiting))
	}
	d.env.clk.Advance(time.Minute)
	d.tick(t)
	if got, want := seenDeferrals(d.memo), slices.Sorted(slices.Values(waiting)); !slices.Equal(got, want) {
		t.Fatalf("seen after pass 2 = %v, want only the two creates still never started %v: the started one is not in it", got, want)
	}
}

// startOptions ties a reconcile run to the memo: nothing for a nil memo, a
// memo-less run on a partial read, and the snapshot's never-started creates
// otherwise.
func TestStartDeferralMemoStartOptions(t *testing.T) {
	apply := func(opts []startExecutionOption, memo *startDeferralMemo) startExecutionOptions {
		got := startExecutionOptions{deferralMemo: memo}
		for _, o := range opts {
			o(&got)
		}
		return got
	}
	infos := []sessionpkg.Info{
		{ID: "never", PendingCreateClaim: true},
		{ID: "never-blank-woke", PendingCreateClaim: true, LastWokeAt: "  "},
		{ID: "started", PendingCreateClaim: true, LastWokeAt: "2026-03-08T12:00:00Z"},
		{ID: "no-claim"},
	}

	t.Run("a nil memo adds nothing", func(t *testing.T) {
		var memo *startDeferralMemo
		if got := memo.startOptions(false, infos); len(got) != 0 {
			t.Fatalf("nil memo returned %d options, want none", len(got))
		}
		if got := memo.startOptions(true, infos); len(got) != 0 {
			t.Fatalf("nil memo returned %d options on a partial read, want none", len(got))
		}
	})

	t.Run("a partial read drops the memo and offers no snapshot", func(t *testing.T) {
		memo := newStartDeferralMemo()
		got := apply(memo.startOptions(true, infos), memo)
		if got.deferralMemo != nil || got.deferralSeen != nil {
			t.Fatalf("partial read left memo=%v seen=%v, want neither", got.deferralMemo, got.deferralSeen)
		}
	})

	t.Run("a full read offers the never-started creates and keeps the memo", func(t *testing.T) {
		memo := newStartDeferralMemo()
		got := apply(memo.startOptions(false, infos), memo)
		if got.deferralMemo != memo {
			t.Fatal("a full read dropped the memo")
		}
		if want := []string{"never", "never-blank-woke"}; !slices.Equal(slices.Sorted(maps.Keys(got.deferralSeen)), want) {
			t.Fatalf("seen = %v, want %v", slices.Sorted(maps.Keys(got.deferralSeen)), want)
		}
	})

	t.Run("a full read of no rows is still an observation", func(t *testing.T) {
		memo := newStartDeferralMemo()
		got := apply(memo.startOptions(false, nil), memo)
		if got.deferralSeen == nil {
			t.Fatal("seen is nil: an empty snapshot would publish nothing and keep a stale record")
		}
	})
}
