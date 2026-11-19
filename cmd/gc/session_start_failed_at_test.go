package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// lastStartFailedAt is the advisory stamp a start failed in prepare or
// pre_start leaves on its row (ga-z4joq0).
const lastStartFailedAt = "last_start_failed_at"

// TestWakeFairnessTimeRanksAFailedStartNoEarlierThanItsFailure pins the
// wake-fairness key's failure term: a session whose start last failed ranks
// no earlier than the failure, so its cleared last_woke_at does not send it
// back to the front of a contended budget ahead of sessions that have waited
// longer since their last attempt.
func TestWakeFairnessTimeRanksAFailedStartNoEarlierThanItsFailure(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name string
		info session.Info
		want time.Time
	}{
		{"failure after the sleep", session.Info{SleptAt: at(-2 * time.Hour), LastStartFailedAt: at(-time.Minute)}, now.Add(-time.Minute)},
		{"failure before the last wake", session.Info{LastWokeAt: at(-time.Minute), LastStartFailedAt: at(-2 * time.Hour)}, now.Add(-time.Minute)},
		{"failure after the creation", session.Info{CreatedAt: now.Add(-3 * time.Hour), LastStartFailedAt: at(-time.Minute)}, now.Add(-time.Minute)},
		{"failure with no other time", session.Info{LastStartFailedAt: at(-time.Minute)}, now.Add(-time.Minute)},
		{"unparseable failure", session.Info{SleptAt: at(-2 * time.Hour), LastStartFailedAt: "garbage"}, now.Add(-2 * time.Hour)},
		{"no failure", session.Info{SleptAt: at(-2 * time.Hour)}, now.Add(-2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wakeFairnessTime(startCandidate{info: tc.info}); !got.Equal(tc.want) {
				t.Fatalf("wakeFairnessTime = %s, want %s", got, tc.want)
			}
		})
	}

	for _, standing := range []bool{false, true} {
		failed := startCandidate{standing: standing, info: session.Info{ID: "failed", SleptAt: at(-2 * time.Hour), LastStartFailedAt: at(-time.Minute)}}
		waiting := startCandidate{standing: standing, info: session.Info{ID: "waiting", LastWokeAt: at(-30 * time.Minute)}}
		ready := []startCandidate{failed, waiting}
		sortCandidatesByWakeFairness(ready)
		if ready[0].info.ID != "waiting" {
			t.Fatalf("standing=%v: order %s, %s; want the session waiting since before the failure first", standing, ready[0].info.ID, ready[1].info.ID)
		}
	}
}

// requireStartFailedStamp fails unless row cleared last_woke_at and stamped
// last_start_failed_at at now.
func requireStartFailedStamp(t *testing.T, row beads.Bead, now time.Time) {
	t.Helper()
	if got := row.Metadata["last_woke_at"]; got != "" {
		t.Errorf("last_woke_at = %q, want cleared", got)
	}
	if got, want := row.Metadata[lastStartFailedAt], now.UTC().Format(time.RFC3339); got != want {
		t.Errorf("%s = %q, want the failure time %q", lastStartFailedAt, got, want)
	}
}

// TestLegacyStartFailureStampsLastStartFailedAt is AC-6 of ga-z4joq0 in the
// legacy reconciler: each arm that ends a start failed in prepare or
// pre_start clears last_woke_at and stamps last_start_failed_at, and the
// failed session then ranks behind a same-class session that has waited
// since before the failure.
func TestLegacyStartFailureStampsLastStartFailedAt(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	t.Run("non-pending failure restores PreWake", func(t *testing.T) {
		e := newCapacityEnv(t, false, "s")
		c := asleepSession(t, e, map[string]string{"slept_at": e.clk.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)})
		e.sp.StartErrors["s"] = boom
		if woken := e.start(ctx, c); woken != 0 {
			t.Fatalf("woken = %d, want the failed start not counted", woken)
		}
		got := e.bead(t, c.info.ID)
		if got.Metadata["state"] != "asleep" {
			t.Fatalf("state = %q, want PreWake restored", got.Metadata["state"])
		}
		requireStartFailedStamp(t, got, e.clk.Now())

		waiting := e.session(t, "w", "", map[string]string{
			"session_name": "w", "template": "w", "state": "asleep",
			"last_woke_at": e.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		})
		ready := []startCandidate{e.refreshed(c), waiting}
		sortCandidatesByWakeFairness(ready)
		if ready[0].info.ID != waiting.info.ID {
			t.Fatalf("the failed session sorts ahead of one waiting since before its failure")
		}
	})

	t.Run("configured pending create keeps its row", func(t *testing.T) {
		e := newCapacityEnv(t, false, "p")
		c := e.pendingCreate(t, "p", "", nil)
		c.configured = true
		e.sp.StartErrors["p"] = boom
		e.start(ctx, c)
		got := e.bead(t, c.info.ID)
		if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("status %q claim %q, want the configured row kept with its claim", got.Status, got.Metadata["pending_create_claim"])
		}
		requireStartFailedStamp(t, got, e.clk.Now())
	})

	t.Run("prepare failure", func(t *testing.T) {
		e := newCapacityEnv(t, false, "worker")
		e.store = &failSetMetadataStore{MemStore: beads.NewMemStore(), failKey: "session_key"}
		c := e.pendingCreate(t, "worker", "", map[string]string{"state": "creating"})
		c.tp.ResolvedProvider = &config.ResolvedProvider{SessionIDFlag: "--session-id"}
		if woken := e.start(ctx, c); woken != 0 {
			t.Fatalf("woken = %d, want 0 when prepare fails after PreWake", woken)
		}
		requireStartFailedStamp(t, e.bead(t, c.info.ID), e.clk.Now())
		if slices.ContainsFunc(e.sp.Calls, func(c runtime.Call) bool { return c.Method == "Start" }) {
			t.Fatalf("provider calls %+v, want no Start after a failed prepare", e.sp.Calls)
		}
	})
}

// TestAbandonStampsLastStartFailedAt is AC-6 of ga-z4joq0 in v2: the
// abandon's prepare and configured-pending class and its any-other class
// (otherFailurePatch) write last_start_failed_at at the pass's time in the
// write that clears or restores last_woke_at.
func TestAbandonStampsLastStartFailedAt(t *testing.T) {
	k := startKit(t, intentStart, "creating")
	boom := errors.New("boom")
	woke := gatherNow.Format(time.RFC3339)
	creating := poolRow("gc-1", "worker", 1, "creating", "generation", "4", "instance_token", "tok-4", "last_woke_at", woke, "wake_attempts", "2")
	undo := preWakeUndo{
		token: "tok-4", written: session.MetadataPatch{"state": "creating", "last_woke_at": woke},
		restore: session.MetadataPatch{"state": "asleep", "last_woke_at": ""},
	}
	other := startFailureRead{startFailure: startFailure{cause: causeStartError, err: boom, undo: undo, display: "worker"}}
	prepare := other
	prepare.cause = causePrepare
	named := other
	named.pending, named.configured = true, true
	relaunch := other
	relaunch.relaunched = true
	killed := poolRow("gc-1", "worker", 1, "asleep", "generation", "4", "instance_token", "tok-4", "sleep_reason", "killed")
	for _, c := range []struct {
		name       string
		row        beads.Bead
		f          startFailureRead
		clearsWoke bool
	}{
		{"prepare", creating, prepare, true},
		{"configured named pending, not died", creating, named, true},
		{"any other failure", creating, other, true},
		{"any other failure of a relaunch", creating, relaunch, false},
		{"any other failure of a row killed mid-start", killed, other, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			step := abandonStep(abandonView(k, c.row, txRuntime{Class: rtAbsent}, c.f))
			if got := step.Write[lastStartFailedAt]; got != woke {
				t.Fatalf("write %v, want %s = %q", step.Write, lastStartFailedAt, woke)
			}
			if v, ok := step.Write["last_woke_at"]; c.clearsWoke && (!ok || v != "") {
				t.Fatalf("write %v, want last_woke_at cleared in the same write", step.Write)
			}
		})
	}
}

// requireNoStartFailedStamp fails if row carries last_start_failed_at.
func requireNoStartFailedStamp(t *testing.T, row beads.Bead) {
	t.Helper()
	if got, ok := row.Metadata[lastStartFailedAt]; ok {
		t.Fatalf("%s = %q on %s, want no stamp: the start did not fail in prepare or pre_start", lastStartFailedAt, got, row.ID)
	}
}

// TestLegacyStartNotFailedInPrepareOrPreStartLeavesNoStamp is AC-7 of
// ga-z4joq0 in the legacy reconciler: a capacity refusal, a budget,
// dependency or attribution deferral, a superseded PreWake, and a
// provider-terminal, rate-limit or pending-create-rollback failure commit
// without writing last_start_failed_at.
func TestLegacyStartNotFailedInPrepareOrPreStartLeavesNoStamp(t *testing.T) {
	ctx := context.Background()

	t.Run("capacity refusal", func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		c := asleepSession(t, e, nil)
		e.sp.StartErrors["s"] = capacityRefusal("s", "")
		e.start(ctx, c)
		got := e.bead(t, c.info.ID)
		if got.Metadata["state"] != "asleep" {
			t.Fatalf("premise: state = %q, want the refusal to restore PreWake", got.Metadata["state"])
		}
		requireNoStartFailedStamp(t, got)
	})

	t.Run("budget deferral", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		e.limitToOneWake()
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)
		if got := e.start(ctx, a, b); got != 1 {
			t.Fatalf("premise: started %d, want 1 under a budget of 1", got)
		}
		requireNoStartFailedStamp(t, e.bead(t, a.info.ID))
		requireNoStartFailedStamp(t, e.bead(t, b.info.ID))
	})

	t.Run("dependency deferral", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		e.cfg.Agents[1].DependsOn = []string{"a"}
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)
		e.sp.StartErrors["a"] = errors.New("boom")
		e.start(ctx, a, b)
		if e.sp.IsRunning("b") {
			t.Fatalf("premise: b started over its failed dependency")
		}
		requireNoStartFailedStamp(t, e.bead(t, b.info.ID))
	})

	t.Run("attribution deferral", func(t *testing.T) {
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		sp.StartErrors["worker"] = errors.New("launcher exited")
		failIdentityReads(sp, "worker", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
		row, err := store.Create(beads.Bead{Title: "worker", Type: session.BeadType})
		if err != nil {
			t.Fatal(err)
		}
		item := unattributedWorkerStart()
		item.candidate.info.ID = row.ID
		result := executePreparedStartWaveForCity(ctx, []preparedStart{item}, t.TempDir(), sp, nil, nil, 10*time.Second, 1)[0]
		if result.outcome != TraceOutcomeDeferred {
			t.Fatalf("premise: outcome = %q, want the attribution deferral", result.outcome)
		}
		clk := &clock.Fake{Time: time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)}
		commitStartResultTraced(result, sessionFrontDoor(store), clk, events.Discard, 0, io.Discard, io.Discard, nil)
		requireNoStartFailedStamp(t, mustGetBead(t, store, row.ID))
	})

	// The executor takes the row's lease just before PreWake, so a PreWake is
	// superseded only when another holder takes the lease over in between;
	// prepare is driven directly under a lease already taken over.
	t.Run("superseded PreWake", func(t *testing.T) {
		f := newLeaseStartFixture(t, beads.NewMemStore())
		f.cand.lease = f.hold(t)
		defer f.cand.lease.Release()
		if err := f.store.SetMetadataBatch(f.cand.info.ID, map[string]string{session.RuntimeLeaseHolderKey: "other/1/n", session.RuntimeLeaseEpochKey: "9"}); err != nil {
			t.Fatal(err)
		}
		clk := &clock.Fake{Time: time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)}
		if _, err := prepareStartCandidateForCity(f.cand, f.city, "test-city", f.cfg, f.sp, f.store, clk, io.Discard, nil, dispatchOptionSources{}); !errors.Is(err, errPreWakeSuperseded) {
			t.Fatalf("premise: prepare = %v, want errPreWakeSuperseded", err)
		}
		requireNoStartFailedStamp(t, mustGetBead(t, f.store, f.cand.info.ID))
	})

	t.Run("provider terminal", func(t *testing.T) {
		e := newCapacityEnv(t, false, "s")
		c := asleepSession(t, e, nil)
		e.sp.StartErrors["s"] = errors.New("model_not_found: x")
		e.start(ctx, c)
		got := e.bead(t, c.info.ID)
		if got.Metadata["sleep_reason"] == "idle" {
			t.Fatalf("premise: sleep_reason = %q, want the terminal provider error marked", got.Metadata["sleep_reason"])
		}
		requireNoStartFailedStamp(t, got)
	})

	t.Run("rate limit", func(t *testing.T) {
		e := newCapacityEnv(t, false, "s")
		c := asleepSession(t, e, nil)
		commitStartFailure(startResult{prepared: preparedStart{candidate: c}, err: errors.New("died during startup"), rateLimitScreen: true},
			sessionFrontDoor(e.store), e.clk, events.Discard, 0, io.Discard, nil)
		got := e.bead(t, c.info.ID)
		if got.Metadata["quarantined_until"] == "" {
			t.Fatalf("premise: no rate-limit quarantine on %v", got.Metadata)
		}
		requireNoStartFailedStamp(t, got)
	})

	t.Run("pending-create rollback", func(t *testing.T) {
		e := newCapacityEnv(t, false, "p")
		c := e.pendingCreate(t, "p", "", nil)
		e.sp.StartErrors["p"] = errors.New("boom")
		e.start(ctx, c)
		got := e.bead(t, c.info.ID)
		if got.Status != "closed" {
			t.Fatalf("premise: status %q, want the unconfigured pending create rolled back", got.Status)
		}
		requireNoStartFailedStamp(t, got)
	})
}

// TestAbandonWithoutPrepareOrPreStartFailureLeavesNoStamp is AC-7 of
// ga-z4joq0 in v2: a capacity refusal and the abandon's terminal, rate-limit,
// pending-teardown and attribution classes write no last_start_failed_at.
func TestAbandonWithoutPrepareOrPreStartFailureLeavesNoStamp(t *testing.T) {
	k := startKit(t, intentStart, "creating")
	boom := errors.New("boom")
	woke := gatherNow.Format(time.RFC3339)
	creating := poolRow("gc-1", "worker", 1, "creating", "generation", "4", "instance_token", "tok-4", "last_woke_at", woke, "wake_attempts", "2")
	undo := preWakeUndo{
		token: "tok-4", written: session.MetadataPatch{"state": "creating", "last_woke_at": woke},
		restore: session.MetadataPatch{"state": "asleep", "last_woke_at": ""},
	}
	other := startFailureRead{startFailure: startFailure{cause: causeStartError, err: boom, undo: undo, display: "worker"}}
	pending := func(f startFailureRead) startFailureRead { f.pending = true; return f }
	terminal := other
	terminal.terminal = "model_not_found"
	limited := other
	limited.rateLimit = true
	namedDied := pending(other)
	namedDied.configured, namedDied.died, namedDied.cause = true, true, causeDiedDuringStartup
	timedOut := pending(other)
	timedOut.err = errors.Join(boom, context.DeadlineExceeded)
	unattributed := pending(other)
	unattributed.cause = causeAttribution
	for _, c := range []struct {
		name string
		f    startFailureRead
	}{
		{"provider terminal", terminal},
		{"rate limit", limited},
		{"pending-create rollback", pending(other)},
		{"pending-create rollback, timed out", timedOut},
		{"pending-create rollback, terminal", pending(terminal)},
		{"configured named pending, died", namedDied},
		{"attribution deferral", unattributed},
	} {
		t.Run(c.name, func(t *testing.T) {
			step := abandonStep(abandonView(k, creating, txRuntime{Class: rtAbsent}, c.f))
			if got, ok := step.Write[lastStartFailedAt]; ok {
				t.Fatalf("write %v carries %s = %q, want no stamp", step.Write, lastStartFailedAt, got)
			}
		})
	}

	t.Run("capacity refusal", func(t *testing.T) {
		now := gatherNow
		kit := failingLaunch(t, fmt.Errorf("quota: %w", runtime.ErrProviderCapacity), "asleep", "sleep_reason", "idle")
		row := kit.p.World.Census.Rows[kit.it.Key]
		tp := TemplateParams{TemplateName: "worker", ResolvedProvider: &config.ResolvedProvider{Name: "test-agent"}}
		kit.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
		kit.p.held.start.capacity, kit.it.Endpoint = newEndpointCapacityGuard(func() time.Time { return now }), endpointKey("provider:test-agent")
		if s := kit.runKind(); s.Cause != causeCapacity {
			t.Fatalf("premise: settlement %+v, want a capacity refusal", s)
		}
		if got := kit.meta(lastStartFailedAt); got != "" {
			t.Fatalf("%s = %q after a capacity refusal, want no stamp", lastStartFailedAt, got)
		}
	})
}
