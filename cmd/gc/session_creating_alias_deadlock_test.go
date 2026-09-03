package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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

// stuckCreatingBead builds the specimen row: a pending create that still holds
// its alias, with a persisted command that has drifted from the template.
func stuckCreatingBead(t *testing.T, store beads.Store, state string) beads.Bead {
	t.Helper()
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
			"pending_create_started_at": pendingCreateStartedAtNow(time.Now().UTC()),
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

	rescued, err := rescuePendingCreateForReset(store, bead.ID, clock.Real{}.Now().UTC(), ioDiscard{})
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
	rescued, err := rescuePendingCreateForReset(store, bead.ID, clock.Real{}.Now().UTC(), ioDiscard{})
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
