package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// ga-6wkhl fixtures. The specimen was a named session (alias "olivia") whose
// bead sat in state=creating for 2.5 h across 111 identical retries: the
// persisted command carried an old model flag, the template resolved a new one,
// and the async-start command-drift gate discarded the start forever while the
// row kept its alias. Nothing on the discard path rewrites a pending create's
// persisted command, so the compare could never converge.
const (
	deadlockAlias          = "olivia"
	deadlockStaleCommand   = "manifold-claude --model claude-opus-4-8"
	deadlockCurrentCommand = "manifold-claude --model claude-opus-5"
)

// deadlockStuckFor is how long the specimen sat in creating before it was
// cleared by hand: 111 retries across 2.5 h. The fixture is aged to match, so a
// row built here is genuinely abandoned — its in-flight lease long expired —
// rather than a spawn that is merely young.
const deadlockStuckFor = 150 * time.Minute

// stuckCreatingBead builds the specimen row: a pending create that still holds
// its alias, with a persisted command that has drifted from the template.
func stuckCreatingBead(t *testing.T, store beads.Store, state string) beads.Bead {
	t.Helper()
	stuckSince := time.Now().UTC().Add(-deadlockStuckFor)
	bead, err := store.Create(beads.Bead{
		Title:  deadlockAlias,
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"state":                     state,
			"alias":                     deadlockAlias,
			"agent_name":                deadlockAlias,
			"session_name":              "s-" + deadlockAlias,
			"session_name_explicit":     "true",
			"instance_token":            "tok-1",
			"generation":                "1",
			"command":                   deadlockStaleCommand,
			"pending_create_claim":      "true",
			"pending_create_started_at": pendingCreateStartedAtNow(stuckSince),
			"last_woke_at":              stuckSince.Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("create stuck session bead: %v", err)
	}
	return bead
}

func driftedStartResult(t *testing.T, bead beads.Bead) startResult {
	t.Helper()
	return startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				info: sessiontest.SeedBead(t, bead),
				tp: TemplateParams{
					TemplateName: "worker",
					Command:      deadlockCurrentCommand,
				},
			},
		},
		outcome: "success",
	}
}

// aliasFree reports whether a fresh owner could claim the alias right now.
// This is the exact serialization point that blocked the specimen's
// replacement: `session beads: alias "olivia" ... already belongs to <id>`.
func aliasFree(store beads.Store) error {
	return sessionpkg.EnsureAliasAvailableWithConfigForOwner(store, &config.City{}, deadlockAlias, "replacement-id", deadlockAlias)
}

// TestAsyncStartDriftRollsBackPendingCreate is the ga-6wkhl specimen pin.
//
// A pending create whose persisted command has drifted from the template
// command must be ROLLED BACK, not discarded as "superseded". The discard
// disposition keeps both the pending-create claim and the alias, and because
// only a completed create ever rewrites the persisted command, the very next
// tick recomputes the identical drift — the row can never converge and nothing
// may replace it. Rollback releases the claim AND the alias so the next tick
// recreates the row against the current template command.
func TestAsyncStartDriftRollsBackPendingCreate(t *testing.T) {
	for _, state := range []string{string(sessionpkg.StateCreating), string(sessionpkg.StateStartPending)} {
		t.Run(state, func(t *testing.T) {
			isolatedAsyncStartFailures(t)
			store := beads.NewMemStore()
			bead := stuckCreatingBead(t, store, state)
			if err := aliasFree(store); err == nil {
				t.Fatal("precondition: the stuck row must own the alias")
			}

			committed := commitAsyncStartResultWithContext(
				context.Background(), driftedStartResult(t, bead), nil, store,
				clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil)
			if committed {
				t.Fatal("commitAsyncStartResultWithContext committed a drifted start; want the start discarded")
			}

			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("re-reading the rolled-back bead: %v", err)
			}
			if got.Metadata["pending_create_claim"] == "true" {
				t.Error("pending_create_claim still set: the drifted pending create was not rolled back, so the next tick repeats the same discard")
			}
			if err := aliasFree(store); err != nil {
				t.Errorf("alias %q still held after the drift discard: %v — this is the ga-6wkhl deadlock: the row cannot converge and nothing may replace it", deadlockAlias, err)
			}
		})
	}
}

// TestAsyncStartDriftKeepsSupersededVerdictForLiveRow is the negative control
// for the specimen pin. A row whose create has ALREADY committed (a live state,
// no pending-create claim) must keep today's behavior exactly: "desired
// command changed" correctly means "do not commit this stale start", and the
// config-drift lane owns drain-and-restart. The rollback must never close a
// live session's bead or release a live session's alias.
func TestAsyncStartDriftKeepsSupersededVerdictForLiveRow(t *testing.T) {
	isolatedAsyncStartFailures(t)
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  deadlockAlias,
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"state":          string(sessionpkg.StateActive),
			"alias":          deadlockAlias,
			"agent_name":     deadlockAlias,
			"session_name":   deadlockAlias,
			"instance_token": "tok-1",
			"generation":     "1",
			"command":        deadlockStaleCommand,
			// No pending_create_claim: this create committed long ago.
		},
	})
	if err != nil {
		t.Fatalf("create live session bead: %v", err)
	}

	if commitAsyncStartResultWithContext(
		context.Background(), driftedStartResult(t, bead), nil, store,
		clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("a drifted start must not commit even for a live row")
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-reading the live bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("the drift rollback closed a LIVE session bead; rollback is only ever correct for a create that never committed")
	}
	if err := aliasFree(store); err == nil {
		t.Fatal("the drift rollback released a LIVE session's alias; live rows must keep today's superseded behavior exactly")
	}
}

// TestPreWakePatchDoesNotRenewPendingCreateStartedAt is the ga-6wkhl bound pin.
//
// preWakeCommit applies PreWakePatch immediately before EVERY runtime start
// attempt. Re-stamping pending_create_started_at there resets the very clock
// that measures "how long has this attempt been stuck", so
// pendingCreateAttemptStaleInfo reads eternally-fresh and the
// staleCreatingStateTimeout reaper is unreachable for a row that retries every
// tick. That is what turned a 2-minute self-heal into 2.5 hours. The stamp must
// mark the START of the pending-create episode and never be renewed while the
// episode is still open.
func TestPreWakePatchDoesNotRenewPendingCreateStartedAt(t *testing.T) {
	episodeStart := time.Date(2026, 9, 2, 23, 17, 0, 0, time.UTC)
	now := episodeStart.Add(2*time.Hour + 37*time.Minute)

	patch := sessionpkg.PreWakePatch(sessionpkg.PreWakePatchInput{
		Generation:                     2,
		InstanceToken:                  "tok-2",
		ContinuationEpoch:              1,
		Now:                            now,
		ExistingPendingCreateStartedAt: episodeStart.Format(time.RFC3339),
	})
	if got := patch["pending_create_started_at"]; got != episodeStart.Format(time.RFC3339) {
		t.Fatalf("pending_create_started_at = %q, want the episode start %q — a renewed bound is not a bound, and the stale-creating reaper can never fire",
			got, episodeStart.Format(time.RFC3339))
	}
}

// TestPreWakePatchStampsPendingCreateStartedAtOnNewEpisode is the complement:
// a genuinely new pending-create episode (no carried timestamp, because every
// commit/sleep/close path clears it) must stamp the current time, or the reaper
// would fall back to CreatedAt and reap a brand-new attempt immediately.
func TestPreWakePatchStampsPendingCreateStartedAtOnNewEpisode(t *testing.T) {
	now := time.Date(2026, 9, 3, 1, 0, 0, 0, time.UTC)
	patch := sessionpkg.PreWakePatch(sessionpkg.PreWakePatchInput{
		Generation:        2,
		InstanceToken:     "tok-2",
		ContinuationEpoch: 1,
		Now:               now,
	})
	if got := patch["pending_create_started_at"]; got != now.Format(time.RFC3339) {
		t.Fatalf("pending_create_started_at = %q, want %q for a fresh pending-create episode", got, now.Format(time.RFC3339))
	}
}

// TestHealthyInFlightCreateIsNotReaped is the design's pin 2, the NEGATIVE
// CONTROL for the bound. Not renewing pending_create_started_at means the
// marker now measures the whole pending-create episode rather than the latest
// attempt, so the one-minute window is reachable for the first time. What keeps
// it from racing a legitimately slow spawn is a different, per-attempt lease:
// pendingCreateStartInFlightInfo leases against the CONFIGURED
// session.startup_timeout off last_woke_at, and every rollback path consults it
// before pendingCreateAttemptStaleInfo.
//
// So a create whose episode marker is long expired but whose current attempt is
// still inside its startup budget must NOT be reaped. This is the pin that lets
// staleCreatingStateTimeout stay at one minute: raising it would only hold
// aliases longer, which is what ga-6wkhl is trying to stop.
func TestHealthyInFlightCreateIsNotReaped(t *testing.T) {
	now := time.Now().UTC()
	budget := (&config.SessionConfig{}).StartupTimeoutDuration()
	info := sessiontest.InfoFromMeta(t, map[string]string{
		"session_name":         deadlockAlias,
		"state":                string(sessionpkg.StateCreating),
		"pending_create_claim": "true",
		// The episode opened long ago and is NOT renewed any more...
		"pending_create_started_at": pendingCreateStartedAtNow(now.Add(-2 * time.Hour)),
		// ...but this attempt started moments ago and is still spawning.
		"last_woke_at": now.Add(-5 * time.Second).Format(time.RFC3339),
	})
	clk := &clock.Fake{Time: now}
	if !pendingCreateAttemptStaleInfo(info, clk) {
		t.Fatal("precondition: with renewal removed the episode marker must read stale")
	}
	if !pendingCreateStartInFlightInfo(info, clk, budget) {
		t.Fatalf("a create whose attempt started 5s ago was not judged in-flight against a %s startup budget; the unrenewed bound would reap a healthy spawn", budget)
	}
	if pendingCreateLeaseActiveInfo(info, clk, budget) != true {
		t.Fatal("pendingCreateLeaseActiveInfo=false for a create still inside its startup budget; the in-flight lease must win over the episode bound")
	}
}

// TestExhaustedInFlightCreateLosesItsLease is the complement: once the attempt
// has outlived its startup budget, the in-flight lease stops protecting it and
// the (now un-renewed) episode bound collects it. Without this the fix would
// swap one unreachable bound for another.
func TestExhaustedInFlightCreateLosesItsLease(t *testing.T) {
	now := time.Now().UTC()
	budget := (&config.SessionConfig{}).StartupTimeoutDuration()
	info := sessiontest.InfoFromMeta(t, map[string]string{
		"session_name":              deadlockAlias,
		"state":                     string(sessionpkg.StateCreating),
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(now.Add(-2 * time.Hour)),
		"last_woke_at":              now.Add(-2 * time.Hour).Format(time.RFC3339),
	})
	clk := &clock.Fake{Time: now}
	if pendingCreateStartInFlightInfo(info, clk, budget) {
		t.Fatal("an attempt two hours past its startup budget still reads in-flight")
	}
	if pendingCreateLeaseActiveInfo(info, clk, budget) {
		t.Fatal("pendingCreateLeaseActiveInfo=true for an exhausted attempt; the episode bound must now collect it")
	}
}

// TestStuckCreatingIsReapedOnceRenewalStops is the design's pin 3. Once the
// re-stamp no longer renews, a creating row older than the bound is reaped.
func TestStuckCreatingIsReapedOnceRenewalStops(t *testing.T) {
	now := time.Now().UTC()
	info := sessiontest.InfoFromMeta(t, map[string]string{
		"session_name":              deadlockAlias,
		"state":                     string(sessionpkg.StateCreating),
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(now.Add(-staleCreatingStateTimeout - time.Second)),
	})
	if !pendingCreateAttemptStaleInfo(info, &clock.Fake{Time: now}) {
		t.Fatal("a pending create older than staleCreatingStateTimeout must be reaped")
	}
}

// TestCommitEndsPendingCreateEpisode is the pin that makes preserving
// pending_create_started_at safe. The marker is only preserved for as long as
// the episode is unresolved; a successful create must clear it, or the next
// legitimate wake would inherit a long-expired timestamp and be reaped
// instantly. This is the invariant "preserve, don't renew" depends on.
func TestCommitEndsPendingCreateEpisode(t *testing.T) {
	now := time.Date(2026, 9, 3, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		input sessionpkg.CommitStartedPatchInput
	}{
		{"confirm state", sessionpkg.CommitStartedPatchInput{Now: now, ConfirmState: true}},
		{"clear claim", sessionpkg.CommitStartedPatchInput{Now: now, ClearPendingCreateClaim: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch := sessionpkg.CommitStartedPatch(tc.input)
			if got, ok := patch["pending_create_started_at"]; !ok || got != "" {
				t.Fatalf("pending_create_started_at = %q (present=%v), want cleared: a committed create must end the episode so the next wake re-stamps", got, ok)
			}
		})
	}
}

// TestSessionNameSurvivesCommit is the design's rename pin. The specimen's
// `session_name=s-<id>` placeholder surviving onto the rebuilt row was a
// CONSEQUENCE of the deadlock — the rename to the chair name happens on commit
// and commit never ran. Nothing in the fix should change that, so pin that a
// committing create does not clear or rewrite the session name.
func TestSessionNameSurvivesCommit(t *testing.T) {
	patch := sessionpkg.CommitStartedPatch(sessionpkg.CommitStartedPatchInput{
		Now:                     time.Date(2026, 9, 3, 1, 0, 0, 0, time.UTC),
		ConfirmState:            true,
		ClearPendingCreateClaim: true,
	})
	if got, ok := patch["session_name"]; ok {
		t.Fatalf("CommitStartedPatch writes session_name = %q; the commit path must leave the established runtime identity alone", got)
	}
	if got := patch["state"]; got != string(sessionpkg.StateActive) {
		t.Fatalf("state = %q, want active — a committed create is the only thing that ends the creating state", got)
	}
}

// TestRollbackReleasesSessionNameForExplicitlyNamedRow pins the other half of
// the rename story: the rollback must surrender the placeholder session_name of
// an explicitly-named row, so the recreated row is free to take the chair name.
func TestRollbackReleasesSessionNameForExplicitlyNamedRow(t *testing.T) {
	isolatedAsyncStartFailures(t)
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))

	commitAsyncStartResultWithContext(
		context.Background(), driftedStartResult(t, bead), nil, store,
		clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-reading the rolled-back bead: %v", err)
	}
	if strings.TrimSpace(got.Metadata["session_name"]) != "" {
		t.Errorf("session_name = %q after rollback; the placeholder must be surrendered so the replacement can claim the chair name", got.Metadata["session_name"])
	}
}

// TestSessionResetRescuesStuckCreatingRow is the design's pin 5. Operators
// reached for `gc session reset` first and it silently did nothing: it resets
// the circuit breaker and the provider conversation state but never clears
// pending_create_claim / pending_create_started_at and never releases the
// alias, so the next tick re-entered the identical drift compare. Reset must
// route through the same rollback.
func TestSessionResetRescuesStuckCreatingRow(t *testing.T) {
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))

	rescued, err := rescuePendingCreateForReset(store, runtime.NewFake(), sessionResetRescueBudget(nil), bead.ID, clock.Real{}, ioDiscard{})
	if err != nil {
		t.Fatalf("rescuePendingCreateForReset: %v", err)
	}
	if !rescued {
		t.Fatal("reset did not rescue a stuck pending create; operators reach for reset first and it must work")
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-reading the reset bead: %v", err)
	}
	if got.Metadata["pending_create_claim"] == "true" {
		t.Error("reset left pending_create_claim set")
	}
	if err := aliasFree(store); err != nil {
		t.Errorf("reset left the alias held: %v", err)
	}
}

// TestSessionResetLeavesLiveRowAlone is the reset negative control: reset must
// keep its existing semantics (restart in place, bead preserved) for a session
// that is not a stuck pending create.
func TestSessionResetLeavesLiveRowAlone(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  deadlockAlias,
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"state":        string(sessionpkg.StateActive),
			"alias":        deadlockAlias,
			"session_name": deadlockAlias,
		},
	})
	if err != nil {
		t.Fatalf("create live bead: %v", err)
	}
	rescued, err := rescuePendingCreateForReset(store, runtime.NewFake(), sessionResetRescueBudget(nil), bead.ID, clock.Real{}, ioDiscard{})
	if err != nil {
		t.Fatalf("rescuePendingCreateForReset: %v", err)
	}
	if rescued {
		t.Fatal("reset rolled back a LIVE session; the rescue path is only for a create that never committed")
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-reading the live bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("reset closed a live session bead")
	}
}

// TestAsyncStartDriftRollbackEmitsOneEvent is the design's pin 6. 111 identical
// failures produced no event at all — logLifecycleOutcome writes stderr only.
// The rollback must emit exactly one typed event per transition (the row is
// closed and recreated, so a stuck row is one event, not 111) and must never
// put the resolved command on the wire, which carries provider credentials in
// argv.
func TestAsyncStartDriftRollbackEmitsOneEvent(t *testing.T) {
	isolatedAsyncStartFailures(t)
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	rec := &memRecorder{}

	commitAsyncStartResultWithContext(
		context.Background(), driftedStartResult(t, bead), nil, store,
		clock.Real{}, rec, 0, ioDiscard{}, ioDiscard{}, nil)

	if !rec.hasType(events.SessionAsyncStartDriftRolledBack) {
		t.Fatalf("no %s event recorded; 111 silent failures is the ga-gg4mv hole", events.SessionAsyncStartDriftRolledBack)
	}
	if len(rec.events) != 1 {
		t.Fatalf("recorded %d events, want exactly 1 per transition", len(rec.events))
	}
	payload := rec.events[0].Payload
	if payload == nil {
		t.Fatal("drift-rollback event carries no typed payload")
	}
	if strings.Contains(string(payload), "claude-opus-4-8") || strings.Contains(string(payload), "claude-opus-5") {
		t.Fatalf("the resolved command leaked onto the event wire: %s — session argv carries provider credentials", payload)
	}
}

// TestConsecutiveAsyncStartFailuresEscalate pins the escalation counter: a
// single failure is not escalated, but a run of them is, so a row that fails
// without ever reaching the rollback arm still becomes visible.
func TestConsecutiveAsyncStartFailuresEscalate(t *testing.T) {
	tracker := newAsyncStartFailureTracker()
	id := "gcg-5436965277210617"
	for i := 1; i < asyncStartFailureEscalationThreshold; i++ {
		if tracker.record(id) {
			t.Fatalf("escalated after %d consecutive failures, want escalation only at %d", i, asyncStartFailureEscalationThreshold)
		}
	}
	if !tracker.record(id) {
		t.Fatalf("no escalation at %d consecutive failures", asyncStartFailureEscalationThreshold)
	}
	// A success clears the run: the next failure starts a fresh count.
	tracker.clear(id)
	if tracker.record(id) {
		t.Fatal("escalated on the first failure after a success; the counter must be consecutive-only")
	}
}

// --- Review round 2: fences on the destructive rollback arm ---

// liveRuntimeFor starts a fake runtime that identity-matches info, so
// runningSessionMatchesPendingCreateInfo confirms it belongs to this session.
func liveRuntimeFor(t *testing.T, name string, info sessionpkg.Info) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("start fake runtime: %v", err)
	}
	if err := sp.SetMeta(name, "GC_SESSION_ID", info.ID); err != nil {
		t.Fatalf("set GC_SESSION_ID: %v", err)
	}
	if err := sp.SetMeta(name, "GC_INSTANCE_TOKEN", info.InstanceToken); err != nil {
		t.Fatalf("set GC_INSTANCE_TOKEN: %v", err)
	}
	return sp
}

// stubbornProvider is a provider whose Stop never actually removes the session:
// the shape stopStaleAsyncStartRuntime silently tolerates today (it swallows any
// non-IsSessionGone Stop error and never re-probes).
type stubbornProvider struct {
	runtime.Provider
}

func (p *stubbornProvider) Stop(string) error { return errors.New("tmux: server not responding") }

// TestAsyncStartDriftRollbackRequiresIdentityMatch is the CRITICAL fence.
//
// The drift gate is evaluated BEFORE the identity/lease fence. That ordering was
// harmless while the drift arm wrote nothing, but the rollback arm closes the
// bead and frees the alias. A late attempt A (tok-1) whose in-flight lease has
// expired must never destroy the identity of the NEWER incarnation B (tok-2)
// that the reconciler has since spawned: A does not own that row, and
// stopStaleAsyncStartRuntime correctly refuses to stop B's runtime, so a
// rollback here would strand a live agent under a freed alias.
func TestAsyncStartDriftRollbackRequiresIdentityMatch(t *testing.T) {
	isolatedAsyncStartFailures(t)
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	// The row has moved on to incarnation B.
	if err := store.SetMetadata(bead.ID, "instance_token", "tok-2"); err != nil {
		t.Fatalf("advance instance_token: %v", err)
	}

	// Attempt A was enqueued against the older incarnation.
	result := driftedStartResult(t, bead)
	result.prepared.candidate.info.InstanceToken = "tok-1"

	if commitAsyncStartResultWithContext(
		context.Background(), result, nil, store,
		clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("a drifted start must not commit")
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-reading the bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("a stale attempt closed the bead of a NEWER incarnation it does not own")
	}
	if err := aliasFree(store); err == nil {
		t.Fatal("a stale attempt released the alias of a newer, live incarnation")
	}
	if strings.TrimSpace(got.Metadata["pending_create_claim"]) != "true" {
		t.Error("a stale attempt cleared the newer incarnation's pending-create claim")
	}
}

// TestAsyncStartDriftRollbackKeepsAliasWhenRuntimeSurvives pins MAJOR 3. The
// rollback frees the alias, so it may only run once the runtime this start
// spawned is confirmed gone. stopStaleAsyncStartRuntime swallows a failed Stop;
// releasing the alias anyway strands a live agent holding the chair name with no
// bead owning it, and every replacement start then hits ErrSessionExists.
func TestAsyncStartDriftRollbackKeepsAliasWhenRuntimeSurvives(t *testing.T) {
	isolatedAsyncStartFailures(t)
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	result := driftedStartResult(t, bead)
	name := result.prepared.candidate.name()
	sp := &stubbornProvider{Provider: liveRuntimeFor(t, name, result.prepared.candidate.info)}

	if commitAsyncStartResultWithContext(
		context.Background(), result, sp, store,
		clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("a drifted start must not commit")
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-reading the bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("the bead was closed while its runtime is still alive: the alias is now free but a live agent still holds the chair name")
	}
	if err := aliasFree(store); err == nil {
		t.Fatal("the alias was released while the spawned runtime survived the stop")
	}
}

// postStopProvider is the identity-confirmed branch's runtime: the identity
// probe matches (the embedded Fake carries this start's GC_SESSION_ID and
// GC_INSTANCE_TOKEN), Stop reports success, and the two existence probes then
// disagree about what that success proved.
type postStopProvider struct {
	runtime.Provider
	name string
	// stopErr is what Stop reports. nil models ssh, whose Stop discards the
	// remote kill-session exit code for idempotence and returns nil whenever
	// the transport itself worked.
	stopErr error
	// isRunning is the bare-bool answer. false is "gone OR I could not look".
	isRunning bool
	// listErr / stillListed are the error-carrying probe's two answers.
	listErr     error
	stillListed bool
}

func (p *postStopProvider) Stop(string) error     { return p.stopErr }
func (p *postStopProvider) IsRunning(string) bool { return p.isRunning }
func (p *postStopProvider) ListRunning(prefix string) ([]string, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	if p.stillListed && strings.HasPrefix(p.name, prefix) {
		return []string{p.name}, nil
	}
	return nil, nil
}

// TestAsyncStartDriftRollbackPostStopConfirmation pins both halves of the
// post-stop asymmetry.
//
// The unconfirmable branch was fixed to require POSITIVE absence, but the
// identity-confirmed branch of the same function kept a bare !IsRunning as its
// proof — one function holding two standards for one question, with the weaker
// one guarding the branch that just tried to kill something. The fix consults
// the error-carrying probe's ERROR here, and deliberately not its absence
// answer; both halves need a pin because each direction has its own regression.
func TestAsyncStartDriftRollbackPostStopConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		sp   func(name string) *postStopProvider
		// rolledBack is whether the rollback may complete: close the bead and
		// free the alias.
		rolledBack bool
	}{
		{
			// The ssh blip. Stop returned nil because the transport worked, not
			// because kill-session did; IsRunning then answers false because the
			// connection blipped, not because the session is gone. Acting on
			// that strands a live remote agent under the chair name.
			name:       "stop_succeeded_but_listing_failed",
			rolledBack: false,
			sp: func(name string) *postStopProvider {
				return &postStopProvider{name: name, listErr: errors.New("ssh box: tmux list-sessions exited 1")}
			},
		},
		{
			// The other half of that asymmetry, and the narrowing control for
			// the row above: a PartialListError WITHOUT ServerAbsent is a failed
			// observation and must still refuse. ssh returns exactly this shape
			// (it cannot classify the remote stderr, so it never claims
			// absence), which is what keeps an ssh blip fail-closed while the
			// tmux row below is allowed through.
			name:       "listing_partial_without_server_absent",
			rolledBack: false,
			sp: func(name string) *postStopProvider {
				return &postStopProvider{name: name, listErr: &runtime.PartialListError{
					Err: errors.New("ssh box: tmux list-sessions exited 1"),
				}}
			},
		},
		{
			// The single-session server. This branch's OWN successful stop
			// killed the last session on the tmux server, so the server exited
			// with it and the very next listing reports ServerAbsent. That is
			// positive proof of death, not a failed observation: a session
			// cannot outlive its server, and identity, a nil Stop and IsRunning
			// false are already established. Refusing it re-spawns and re-kills
			// a real agent every tick until the reaper fires.
			name:       "stop_killed_the_last_session",
			rolledBack: true,
			sp: func(name string) *postStopProvider {
				return &postStopProvider{name: name, listErr: &runtime.PartialListError{
					Err:          errors.New("tmux server unreachable: no server running"),
					ServerAbsent: true,
				}}
			},
		},
		{
			// The k8s termination grace window, and the reason the remedy reads
			// only the error. Stop issues a delete with a grace period and
			// returns immediately; a Terminating pod keeps status.phase=Running
			// and therefore stays LISTED until it finally disappears. Requiring
			// positive absence here would refuse every k8s rollback for that
			// whole window and retry next tick forever.
			name:       "k8s_terminating_still_listed",
			rolledBack: true,
			sp: func(name string) *postStopProvider {
				return &postStopProvider{name: name, stillListed: true}
			},
		},
		{
			// The ordinary success: the stop worked and the listing confirms it.
			name:       "stop_confirmed_gone",
			rolledBack: true,
			sp:         func(name string) *postStopProvider { return &postStopProvider{name: name} },
		},
		{
			// The pre-existing guard must survive the new one: a runtime the
			// bare probe positively reports as alive still refuses, without ever
			// reaching the listing.
			name:       "survived_its_stop",
			rolledBack: false,
			sp:         func(name string) *postStopProvider { return &postStopProvider{name: name, isRunning: true} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedAsyncStartFailures(t)
			store := beads.NewMemStore()
			bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
			result := driftedStartResult(t, bead)
			name := result.prepared.candidate.name()
			sp := tc.sp(name)
			sp.Provider = liveRuntimeFor(t, name, result.prepared.candidate.info)

			// Capture the fence's diagnostics rather than discarding them. Four
			// gates in pendingCreateRuntimeClearedForRollback can refuse a
			// rollback and every refusal reaches the assertions below as the
			// same message, so discarding stderr turns a failure here into a
			// four-way ambiguity. Captured, each gate names itself: "stopping
			// runtime ... before releasing its identifiers" (Stop errored),
			// "survived its stop" (still running after Stop), "cannot confirm
			// runtime ... stopped" (the post-stop listing gate), "cannot
			// confirm runtime ... is gone" (the identity-mismatch arm's absence
			// probe). Its one silent refusal — that arm finding the name still
			// listed — is then the case where only the "rolling back pending
			// create" preamble appears. If a row reddens in CI, re-run first;
			// only a reproduction makes this text worth reading.
			var fenceLog bytes.Buffer

			if commitAsyncStartResultWithContext(
				context.Background(), result, sp, store,
				clock.Real{}, events.Discard, 0, &fenceLog, &fenceLog, nil) {
				t.Fatalf("a drifted start must not commit; fence output: %q", fenceLog.String())
			}

			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("re-reading the bead: %v; fence output: %q", err, fenceLog.String())
			}
			closed := got.Status == "closed"
			aliasReleased := aliasFree(store) == nil
			if closed != tc.rolledBack {
				if tc.rolledBack {
					t.Fatalf("the bead is still open: a rollback that is safe to complete was refused, so the row retries forever; fence output: %q", fenceLog.String())
				}
				t.Fatalf("the bead was closed on an UNCONFIRMED stop; a live agent now holds the chair name with no bead owning it; fence output: %q", fenceLog.String())
			}
			if aliasReleased != tc.rolledBack {
				if tc.rolledBack {
					t.Fatalf("the alias is still held after a confirmed stop: the replacement start will fail with ErrSessionExists; fence output: %q", fenceLog.String())
				}
				t.Fatalf("the alias was released without confirming the stop actually took effect; fence output: %q", fenceLog.String())
			}
		})
	}
}

// TestAsyncStartDriftRollbackNoEventWhenRollbackFails pins MINOR: the typed
// event must report what actually happened. A rollback whose transaction fails
// leaves the row holding its claim and alias, so reporting
// async_start_drift_rolled_back would recreate the silent-failure hole the
// observability half of this fix exists to close — and clearing the
// consecutive-failure counter on that path means the escalation can never fire.
func TestAsyncStartDriftRollbackNoEventWhenRollbackFails(t *testing.T) {
	tracker := isolatedAsyncStartFailures(t)
	store := &txErrorStore{MemStore: beads.NewMemStore()}
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	rec := &memRecorder{}

	commitAsyncStartResultWithContext(
		context.Background(), driftedStartResult(t, bead), nil, store,
		clock.Real{}, rec, 0, ioDiscard{}, ioDiscard{}, nil)

	if rec.hasType(events.SessionAsyncStartDriftRolledBack) {
		t.Fatal("emitted a drift-rollback event for a rollback that never landed")
	}
	if tracker.count(bead.ID) == 0 {
		t.Fatal("the consecutive-failure counter was cleared by a rollback that failed; the escalation could never fire")
	}
}

// txErrorStore fails every Tx so the rollback cannot land.
type txErrorStore struct {
	*beads.MemStore
}

func (s *txErrorStore) Tx(string, func(beads.Tx) error) error {
	return errors.New("store: transaction failed")
}

// TestResetRefusesHealthyInFlightCreate pins MAJOR 1+2. Operators reach for
// `gc session reset` exactly when a session looks stuck in creating — which is
// also when a perfectly healthy spawn is mid-flight. Rolling that back closes
// the bead of a live create and frees its alias. The sibling CLI path
// (sessionWakeCreateAbandonedInfo) already requires the lease to have expired
// AND the row to be stale; reset must too.
func TestResetRefusesHealthyInFlightCreate(t *testing.T) {
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	now := time.Now().UTC()
	// A spawn that started three seconds ago: well inside the startup budget.
	if err := store.SetMetadata(bead.ID, "pending_create_started_at", pendingCreateStartedAtNow(now.Add(-3*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(bead.ID, "last_woke_at", now.Add(-3*time.Second).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	// A fake clock pinned to now makes "three seconds into the spawn" exact
	// rather than a race against wall time.
	rescued, err := rescuePendingCreateForReset(store, nil, sessionResetRescueBudget(nil), bead.ID, &clock.Fake{Time: now}, ioDiscard{})
	if err != nil {
		t.Fatalf("rescuePendingCreateForReset: %v", err)
	}
	if rescued {
		t.Fatal("reset rolled back a healthy in-flight create; the controller's spawn was still running")
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" {
		t.Fatal("reset closed the bead of a live create")
	}
	if err := aliasFree(store); err == nil {
		t.Fatal("reset released the alias of a live create")
	}
}

// TestResetRefusesWhenRuntimeStillAlive pins the other half of MAJOR 1: the
// documented post-start ApplyPatch failure leaves a LIVE runtime with the row
// deliberately parked in creating for warm reuse. Reset must not close a
// live human-serving agent's bead and orphan its runtime.
func TestResetRefusesWhenRuntimeStillAlive(t *testing.T) {
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	// stuckCreatingBead is already aged past its lease, so only liveness
	// protects this row.
	info, _, err := sessionFrontDoor(store).GetPersistedResponse(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	sp := &stubbornProvider{Provider: liveRuntimeFor(t, "s-"+deadlockAlias, info)}

	rescued, rescueErr := rescuePendingCreateForReset(store, sp, sessionResetRescueBudget(nil), bead.ID, clock.Real{}, ioDiscard{})
	if rescueErr == nil && rescued {
		t.Fatal("reset closed the bead of a session whose runtime is still alive and could not be stopped")
	}
	got, gErr := store.Get(bead.ID)
	if gErr != nil {
		t.Fatal(gErr)
	}
	if got.Status == "closed" {
		t.Fatal("reset closed a live session's bead, orphaning its runtime under the chair name")
	}
}

// TestResetReportsFailedRollback pins MAJOR 4. Collapsing "not eligible",
// "already closed" and "the rollback transaction FAILED" into (false, nil) makes
// reset print success and exit 0 while the row still holds its claim and alias —
// the exact silent no-op this defect exists to remove.
func TestResetReportsFailedRollback(t *testing.T) {
	store := &txErrorStore{MemStore: beads.NewMemStore()}
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	rescued, err := rescuePendingCreateForReset(store, runtime.NewFake(), sessionResetRescueBudget(nil), bead.ID, clock.Real{}, ioDiscard{})
	if rescued {
		t.Fatal("reported a rescue that never landed")
	}
	if err == nil {
		t.Fatal("an eligible row whose rollback failed reported (false, nil); reset then prints success and exits 0 while the row still holds its alias")
	}
}

// TestHealToAsleepClearsPendingCreateMarker pins MAJOR 5. preWakeCommit stamps
// pending_create_started_at on EVERY start candidate, but pending_create_claim
// is set only by bead creation, named-session reopen and the wake-request
// patches — never by an ordinary wake. clearPendingCreateLeaseInfo is gated on
// the claim, so a claimless creating row healed to asleep keeps the marker. With
// the marker no longer renewed, the next wake would inherit an aged marker, read
// instantly stale, and be flapped back to asleep mid-start.
func TestHealToAsleepClearsPendingCreateMarker(t *testing.T) {
	now := time.Now().UTC()
	info := sessiontest.InfoFromMeta(t, map[string]string{
		"session_name":              deadlockAlias,
		"state":                     string(sessionpkg.StateCreating),
		"pending_create_started_at": pendingCreateStartedAtNow(now.Add(-time.Hour)),
		// No pending_create_claim: this is an ordinary wake's episode.
	})
	batch := healStatePatchWithRollbackInfo(info, false, true, &clock.Fake{Time: now}, time.Minute, true)
	if batch["state"] != string(sessionpkg.StateAsleep) {
		t.Fatalf("precondition: heal must park the stale creating row asleep, got state=%q", batch["state"])
	}
	got, ok := batch["pending_create_started_at"]
	if !ok || got != "" {
		t.Fatalf("pending_create_started_at = %q (present=%v), want cleared: a row leaving the creating episode must surrender its start marker or the next wake reads instantly stale", got, ok)
	}
}

// TestWakeAfterHealStartsAFreshEpisode is the end-to-end complement: a row that
// was healed to asleep must not inherit the old marker on its next wake.
func TestWakeAfterHealStartsAFreshEpisode(t *testing.T) {
	now := time.Now().UTC()
	asleep := sessiontest.InfoFromMeta(t, map[string]string{
		"session_name": deadlockAlias,
		"state":        string(sessionpkg.StateAsleep),
		// A marker left behind by a prior episode.
		"pending_create_started_at": pendingCreateStartedAtNow(now.Add(-time.Hour)),
	})
	patch := sessionpkg.PreWakePatch(sessionpkg.PreWakePatchInput{
		Generation:                     2,
		InstanceToken:                  "tok-2",
		ContinuationEpoch:              1,
		Now:                            now,
		ExistingPendingCreateStartedAt: pendingCreateStartedAtForWake(asleep),
	})
	if got := patch["pending_create_started_at"]; got != now.Format(time.RFC3339) {
		t.Fatalf("pending_create_started_at = %q, want a fresh stamp %q: waking an ASLEEP row opens a new episode, it does not continue the old one", got, now.Format(time.RFC3339))
	}
}

// TestEscalationEventCarriesFingerprints pins MINOR: the escalation exists to
// tell "the same drift is repeating" from "the store is flaky", which is exactly
// what the fingerprints encode. Dropping them makes the two indistinguishable.
func TestEscalationEventCarriesFingerprints(t *testing.T) {
	rec := &memRecorder{}
	emitAsyncStartRefreshStalled(rec, "olivia", "gc-1", "worker", "async_start_refresh_failed",
		asyncStartFailureEscalationThreshold, deadlockCurrentCommand, deadlockStaleCommand, ioDiscard{})
	if len(rec.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.events))
	}
	payload := string(rec.events[0].Payload)
	wantPrepared := commandFingerprint(deadlockCurrentCommand)
	wantCurrent := commandFingerprint(deadlockStaleCommand)
	if wantPrepared == "" || wantCurrent == "" {
		t.Fatal("fixture produced empty fingerprints")
	}
	if !strings.Contains(payload, wantPrepared) || !strings.Contains(payload, wantCurrent) {
		t.Fatalf("escalation payload %s carries no command fingerprints", payload)
	}
	if strings.Contains(payload, "claude-opus") {
		t.Fatalf("raw command leaked onto the escalation wire: %s", payload)
	}
}

// TestAsyncStartFailureTrackerEvictsAndIsBounded pins MINOR: the tracker is
// process-global in a long-lived controller. Entries for beads closed by any
// other lane must not accumulate forever.
func TestAsyncStartFailureTrackerEvictsAndIsBounded(t *testing.T) {
	tracker := newAsyncStartFailureTracker()
	tracker.record("gc-1")
	tracker.forget("gc-1")
	if tracker.count("gc-1") != 0 {
		t.Fatal("forget did not evict the entry")
	}
	for i := 0; i < asyncStartFailureTrackerMaxEntries*2; i++ {
		tracker.record(fmt.Sprintf("gc-%d", i))
	}
	if n := tracker.size(); n > asyncStartFailureTrackerMaxEntries {
		t.Fatalf("tracker holds %d entries, want at most %d", n, asyncStartFailureTrackerMaxEntries)
	}
}

// --- Review round 3: the liveness fence must establish "no live runtime" ---

// isolatedAsyncStartFailures swaps the controller-wide failure counter for one
// this test owns, and restores it with t.Cleanup.
//
// asyncStartFailures is per-process controller state keyed by bead ID, and
// beads.NewMemStore mints IDs per store, so two tests that each build a fresh
// store can collide on one key; a bare clear() at the end of a test is also
// skipped by any t.Fatal above it. Swapping the variable gives each test its own
// tracker without threading one more parameter through
// commitAsyncStartResultWithContext's already-long signature. (Package-global
// swap: these tests must not call t.Parallel.)
//
// EVERY test in this package that drives commitAsyncStartResultWithContext must
// call this, not only the ones that assert on the counter. The escalation's
// default: arm widened the recording population from the releaseInFlight
// disposition alone to every non-commit disposition, so unisolated tests now
// accumulate into one process-global tracker: three of them colliding on a
// MemStore-minted bead id reach asyncStartFailureEscalationThreshold inside the
// third, which then emits a SessionAsyncStartRefreshStalled event and a loud
// stderr line its own scenario never produced. That is invisible until the next
// event-count assertion lands in that file and fails for a reason that has
// nothing to do with it. (Carrying the tracker on the reconciler value instead
// of a package global is the durable fix and remains open.)
func isolatedAsyncStartFailures(t *testing.T) *asyncStartFailureTracker {
	t.Helper()
	previous := asyncStartFailures
	tracker := newAsyncStartFailureTracker()
	asyncStartFailures = tracker
	t.Cleanup(func() { asyncStartFailures = previous })
	return tracker
}

// unobservableRuntimeProvider is the "alive but not yet observable" runtime: a
// k8s pod that is Running while its tmux server is still inside the startup
// grace period, or a tmux server mid-blip. Both probes the rollback guard used
// to consult read that one unavailable signal — GetMeta answers ("", nil) so the
// identity probe fails, and IsRunning execs into the same unstarted tmux so it
// reports false — which made "confirmed gone" the conjunction of two failures.
// Only the error-carrying listing still distinguishes the two.
type unobservableRuntimeProvider struct {
	runtime.Provider
	name    string
	listErr error
	// listNames are the best-effort names returned ALONGSIDE listErr, for the
	// degraded-but-usable shape (runtime.PartialListError): the provider
	// observed some sessions and is reporting that it could not observe the
	// rest. They deliberately never include name.
	listNames []string
}

func (p *unobservableRuntimeProvider) GetMeta(string, string) (string, error) { return "", nil }
func (p *unobservableRuntimeProvider) IsRunning(string) bool                  { return false }

func (p *unobservableRuntimeProvider) ListRunning(prefix string) ([]string, error) {
	if p.listErr != nil {
		return p.listNames, p.listErr
	}
	if strings.HasPrefix(p.name, prefix) {
		return []string{p.name}, nil
	}
	return nil, nil
}

// TestAsyncStartDriftRollbackRefusesUnobservableRuntime pins Finding 1b. The
// rollback closes the bead and frees the alias, so it needs POSITIVE proof the
// runtime is gone. A runtime that cannot be observed is not a runtime that is
// gone, and the two probes the guard consulted first both fail on exactly the
// same unavailability — so the guard failed OPEN in the one window it was
// written to close.
func TestAsyncStartDriftRollbackRefusesUnobservableRuntime(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listErr   error
		listNames []string
	}{
		// The k8s shape: the pod is Running and still listed; only tmux is
		// not answering yet.
		{name: "still_listed"},
		// The tmux shape: the server hiccups, so the listing cannot answer at
		// all. An observation failure is not proof of absence.
		{name: "listing_failed", listErr: errors.New("tmux server unreachable")},
		// The ssh/herdr shape, in the form the Provider.ListRunning contract
		// now requires them to report it: the listing could not observe THIS
		// session and says so with the sanctioned degraded-but-usable signal,
		// while still returning the sessions it did see. Both providers used to
		// fold that failure into a clean list that simply lacked the name —
		// ssh turned a non-zero remote `list-sessions` into ([], nil), herdr
		// dropped a binding whose pane probe errored — which this guard read as
		// positive proof of absence. The best-effort names must NOT be mistaken
		// for a complete answer: the error is the answer.
		{
			name:      "partial_listing",
			listErr:   &runtime.PartialListError{Err: errors.New("pane probe failed: herdr socket closed mid-request")},
			listNames: []string{"some-other-live-session"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedAsyncStartFailures(t)
			store := beads.NewMemStore()
			bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
			result := driftedStartResult(t, bead)
			name := result.prepared.candidate.name()
			sp := &unobservableRuntimeProvider{
				Provider:  liveRuntimeFor(t, name, result.prepared.candidate.info),
				name:      name,
				listErr:   tc.listErr,
				listNames: tc.listNames,
			}

			if commitAsyncStartResultWithContext(
				context.Background(), result, sp, store,
				clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
				t.Fatal("a drifted start must not commit")
			}

			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("re-reading the bead: %v", err)
			}
			if got.Status == "closed" {
				t.Fatal("the bead was closed on an UNCONFIRMED absence; a live agent now holds the chair name with no bead owning it")
			}
			if err := aliasFree(store); err == nil {
				t.Fatal("the alias was released without confirming the runtime is gone: every replacement start now fails with ErrSessionExists")
			}
		})
	}
}

// TestAsyncStartDriftRollbackDefersWhileStartDefersCommit pins Finding 1a. The
// rollback arm is the one destructive disposition in this function and it was
// the only one that did not consult startOutcomeDefersCommit. Both deferring
// outcomes mean "the runtime is there but this tick could not decide about it",
// which commitStartResultTraced already treats as not-yet-safe — so the arm that
// closes the bead and frees the alias must not override that judgement.
func TestAsyncStartDriftRollbackDefersWhileStartDefersCommit(t *testing.T) {
	for _, outcome := range []TraceOutcomeCode{TraceOutcomeSessionInitializing, TraceOutcomeDeferred} {
		t.Run(string(outcome), func(t *testing.T) {
			isolatedAsyncStartFailures(t)
			store := beads.NewMemStore()
			bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
			result := driftedStartResult(t, bead)
			result.outcome = outcome
			name := result.prepared.candidate.name()
			// A plain Fake: its Stop works, so nothing but the outcome gate
			// stands between this runtime and being killed.
			sp := liveRuntimeFor(t, name, result.prepared.candidate.info)

			if commitAsyncStartResultWithContext(
				context.Background(), result, sp, store,
				clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
				t.Fatal("a drifted start must not commit")
			}

			if !sp.IsRunning(name) {
				t.Error("a still-initializing runtime was stopped by the rollback arm; the pre-image deliberately let it finish booting")
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("re-reading the bead: %v", err)
			}
			if got.Status == "closed" {
				t.Fatal("the bead was closed while its runtime was still initializing")
			}
			if err := aliasFree(store); err == nil {
				t.Fatal("the alias was released while its runtime was still initializing")
			}
			// Deferring falls through to the releaseInFlight arm, so the next
			// tick retries rather than the row sitting on a spent lease.
			if strings.TrimSpace(got.Metadata["last_woke_at"]) != "" {
				t.Error("the in-flight lease was not released, so the next tick will not retry the start")
			}
		})
	}
}

// TestResetRefusesStoppableLiveRuntime pins Finding 2. TestResetRefusesWhenRuntimeStillAlive
// covers only a runtime whose Stop ERRORS, so the suite stayed green while reset
// happily stopped every runtime it could. Reset's whole contract is that a live
// runtime is restarted in place; killing one to satisfy its own rollback gate
// destroys the bead, alias, mail and queued-work bindings the in-place restart
// exists to preserve, and its help text promises the opposite.
func TestResetRefusesStoppableLiveRuntime(t *testing.T) {
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	info, _, err := sessionFrontDoor(store).GetPersistedResponse(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	name := "s-" + deadlockAlias
	// A plain runtime.Fake, not stubbornProvider: this Stop succeeds.
	sp := liveRuntimeFor(t, name, info)

	rescued, rescueErr := rescuePendingCreateForReset(store, sp, sessionResetRescueBudget(nil), bead.ID, clock.Real{}, ioDiscard{})
	if rescueErr != nil {
		t.Fatalf("rescuePendingCreateForReset: %v", rescueErr)
	}
	if rescued {
		t.Fatal("reset rolled back a session whose runtime is alive; its help text promises exactly the opposite")
	}
	if !sp.IsRunning(name) {
		t.Error("reset STOPPED a live agent to satisfy its own rollback gate")
	}
	got, gErr := store.Get(bead.ID)
	if gErr != nil {
		t.Fatal(gErr)
	}
	if got.Status == "closed" {
		t.Fatal("reset closed the bead of a live session")
	}
	if err := aliasFree(store); err == nil {
		t.Fatal("reset released the alias of a live session")
	}
}

// TestStaleAttemptDoesNotReleaseTheOwnersLease pins Finding 3. A late attempt
// that fails the identity fence does not own the row any more: last_woke_at is
// the in-flight lease of the incarnation that is currently spawning. The
// still-current gate already withholds the lease clear on an identity mismatch;
// the drift gate runs first, so it has to withhold it too. The exposure is new
// with this PR: pending_create_started_at is no longer re-stamped per attempt,
// so an unprotected spawn gap inside an episode older than the never-started
// bound is reapable, and a healthy create gets rolled back.
func TestStaleAttemptDoesNotReleaseTheOwnersLease(t *testing.T) {
	isolatedAsyncStartFailures(t)
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	// The row has moved on to incarnation B, which is mid-spawn and owns the
	// lease this stale attempt A must not touch.
	if err := store.SetMetadata(bead.ID, "instance_token", "tok-2"); err != nil {
		t.Fatalf("advance instance_token: %v", err)
	}
	leaseBefore := strings.TrimSpace(mustBeadMetadata(t, store, bead.ID)["last_woke_at"])
	if leaseBefore == "" {
		t.Fatal("precondition: the row must carry an in-flight lease")
	}

	result := driftedStartResult(t, bead)
	result.prepared.candidate.info.InstanceToken = "tok-1"
	if commitAsyncStartResultWithContext(
		context.Background(), result, nil, store,
		clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("a drifted start must not commit")
	}

	if got := strings.TrimSpace(mustBeadMetadata(t, store, bead.ID)["last_woke_at"]); got != leaseBefore {
		t.Errorf("last_woke_at = %q, want the untouched %q: a stale attempt released the in-flight lease of the incarnation that owns the row", got, leaseBefore)
	}
}

// TestStaleAsyncStartArmStillEscalates pins Finding 4. The escalation switch had
// no default, so the stale_async_start disposition — the still-current reject,
// reached by an attempt whose row was re-leased mid-start without any command
// drift — neither recorded nor cleared the consecutive-failure counter. It is a
// repeating non-commit arm, so a session looping on it stayed exactly as silent
// as the 111 retries this counter was added to surface.
func TestStaleAsyncStartArmStillEscalates(t *testing.T) {
	tracker := isolatedAsyncStartFailures(t)
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	if err := store.SetMetadata(bead.ID, "instance_token", "tok-2"); err != nil {
		t.Fatalf("advance instance_token: %v", err)
	}
	rec := &memRecorder{}

	for i := 1; i <= asyncStartFailureEscalationThreshold; i++ {
		result := driftedStartResult(t, bead)
		// No drift: the prepared command matches the persisted one, so this
		// reaches the still-current gate rather than the drift gate.
		result.prepared.candidate.tp.Command = deadlockStaleCommand
		result.prepared.candidate.info.InstanceToken = "tok-1"
		if commitAsyncStartResultWithContext(
			context.Background(), result, nil, store,
			clock.Real{}, rec, 0, ioDiscard{}, ioDiscard{}, nil) {
			t.Fatalf("attempt %d: a start whose row was re-leased must not commit", i)
		}
	}

	if n := tracker.count(bead.ID); n != asyncStartFailureEscalationThreshold {
		t.Errorf("consecutive-failure count = %d after %d identical stale_async_start failures, want %d: this arm is invisible to the escalation", n, asyncStartFailureEscalationThreshold, asyncStartFailureEscalationThreshold)
	}
	if !rec.hasType(events.SessionAsyncStartRefreshStalled) {
		t.Fatal("no escalation event after a run of identical stale_async_start failures: the one repeating arm that never becomes visible")
	}
}

// TestResetRefusesFreshEpisodeWithExpiredLease covers the other half of the
// reset eligibility gate. TestResetRefusesHealthyInFlightCreate short-circuits
// on the lease arm, so the staleness arm had no deterministic coverage — and it
// used to read wall-clock time while the lease arm read the injected clock, so
// pinning the clock pinned only half the decision. A row whose lease has expired
// but whose pending-create EPISODE is younger than the stale bound is still a
// healthy create.
func TestResetRefusesFreshEpisodeWithExpiredLease(t *testing.T) {
	store := beads.NewMemStore()
	bead := stuckCreatingBead(t, store, string(sessionpkg.StateCreating))
	now := time.Now().UTC()
	// The episode opened a second ago; the attempt's lease is long gone.
	if err := store.SetMetadata(bead.ID, "pending_create_started_at", pendingCreateStartedAtNow(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}

	rescued, err := rescuePendingCreateForReset(store, nil, sessionResetRescueBudget(nil), bead.ID, &clock.Fake{Time: now}, ioDiscard{})
	if err != nil {
		t.Fatalf("rescuePendingCreateForReset: %v", err)
	}
	if rescued {
		t.Fatal("reset rolled back a create whose episode is younger than the stale-creating bound")
	}
	if err := aliasFree(store); err == nil {
		t.Fatal("reset released the alias of a create still inside its episode bound")
	}
}

// mustBeadMetadata re-reads a bead's metadata or fails the test.
func mustBeadMetadata(t *testing.T, store beads.Store, id string) map[string]string {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("re-reading bead %s: %v", id, err)
	}
	return got.Metadata
}
