package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The startup-kickoff-nudge rows exercise nudgeOutstandingStartupKickoffs,
// the named/direct sibling to the pool claim/continuation and execution
// backstops: it re-nudges a named/direct session whose bound step hasn't
// advanced past an outstanding startup kickoff, and gives up (with a typed
// event) after the shared max-attempts cap.

// wantStartupKickoffGivenUpEventType pins the literal value GREEN is expected
// to register as events.SessionStartupKickoffGivenUp (AC4). RED asserts
// against this literal instead of importing the not-yet-existing events
// symbol, so this step's diff stays entirely inside _test.go files.
const wantStartupKickoffGivenUpEventType = "session.startup_kickoff_given_up"

// startupKickoffGivenUpPayload mirrors the JSON shape GREEN is expected to
// register as events.SessionStartupKickoffGivenUpPayload (AC4).
type startupKickoffGivenUpPayload struct {
	SessionID   string `json:"session_id"`
	BoundStepID string `json:"bound_step_id"`
	Attempts    int    `json:"attempts"`
}

func startupKickoffTestCfg() *config.City {
	return &config.City{Agents: []config.Agent{{
		Name:  "agent-a",
		Nudge: "Run gc hook --claim --json now; if it returns work, execute the claimed formula immediately.",
	}}}
}

func namedDirectSessionBead(boundStepID string, startedAt time.Time) beads.Bead {
	return beads.Bead{
		ID:     "session-bead-a",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name":                  "session-a",
			"template":                      "agent-a",
			beadmeta.BoundStepIDMetadataKey: boundStepID,
			startupKickoffStateKey:          startupKickoffStatePending,
			startupKickoffStartedAtKey:      startedAt.UTC().Format(time.RFC3339),
			startupKickoffAttemptsKey:       "0",
		},
	}
}

func runningStartupKickoffFake(t *testing.T) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "session-a", runtime.Config{}); err != nil {
		t.Fatalf("fake start session-a: %v", err)
	}
	return sp
}

func mustGetStartupKickoffBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return b
}

// AC1 + AC2: the backstop runs at the named/direct gate and re-nudges an
// outstanding startup kickoff using the existing delivery/nudge primitive,
// once the grace window has elapsed.
func TestNudgeOutstandingStartupKickoffs_WaitsThenNudgesAfterGrace(t *testing.T) {
	sp := runningStartupKickoffFake(t)
	cfg := startupKickoffTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := namedDirectSessionBead("step-a", base)
	boundSteps := []beads.Bead{{ID: "step-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	rec := events.NewFake()
	clk := &clock.Fake{Time: base}
	var out bytes.Buffer

	nudgeOutstandingStartupKickoffs(sp, cfg, store, []beads.Bead{session}, boundSteps, clk.Now(), rec, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls inside grace = %d, want 0", got)
	}

	clk.Advance(idleClaimNudgeGrace + time.Second)
	session = mustGetStartupKickoffBead(t, store, session.ID)
	nudgeOutstandingStartupKickoffs(sp, cfg, store, []beads.Bead{session}, boundSteps, clk.Now(), rec, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("Nudge calls after grace = %d, want 1", got)
	}

	session = mustGetStartupKickoffBead(t, store, session.ID)
	if got := session.Metadata[startupKickoffStateKey]; got != startupKickoffStateNudged {
		t.Fatalf("startup kickoff state = %q, want %q", got, startupKickoffStateNudged)
	}
	if got := session.Metadata[startupKickoffAttemptsKey]; got != "1" {
		t.Fatalf("startup kickoff attempts = %q, want %q", got, "1")
	}
	if want := clk.Now().UTC().Format(time.RFC3339); session.Metadata[startupKickoffLastNudgeAtKey] != want {
		t.Fatalf("startup kickoff last nudge at = %q, want %q", session.Metadata[startupKickoffLastNudgeAtKey], want)
	}
}

// AC3: a session whose bound work-step has already advanced past open is a
// no-op and must not be double-nudged.
func TestNudgeOutstandingStartupKickoffs_NoOpWhenBoundStepAlreadyAdvanced(t *testing.T) {
	sp := runningStartupKickoffFake(t)
	cfg := startupKickoffTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := namedDirectSessionBead("step-a", base)
	session.Metadata[startupKickoffStateKey] = startupKickoffStateNudged
	session.Metadata[startupKickoffAttemptsKey] = "1"
	session.Metadata[startupKickoffLastNudgeAtKey] = base.Format(time.RFC3339)
	boundSteps := []beads.Bead{{ID: "step-a", Status: "in_progress"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	rec := events.NewFake()
	now := base.Add(2 * idleClaimNudgeBackoff)
	var out bytes.Buffer

	nudgeOutstandingStartupKickoffs(sp, cfg, store, []beads.Bead{session}, boundSteps, now, rec, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls once the bound step advanced = %d, want 0", got)
	}

	session = mustGetStartupKickoffBead(t, store, session.ID)
	if got := session.Metadata[startupKickoffStateKey]; got != startupKickoffStateConfirmed {
		t.Fatalf("startup kickoff state = %q, want %q", got, startupKickoffStateConfirmed)
	}
	if got := session.Metadata[startupKickoffLastNudgeAtKey]; got != "" {
		t.Fatalf("startup kickoff last nudge at = %q, want cleared so a future rebind starts its own grace window", got)
	}
	if len(rec.Events) != 0 {
		t.Fatalf("recorded events = %d, want 0 (confirming an advanced step is not a give-up)", len(rec.Events))
	}
}

// AC4: exhaustion after the configured max attempts transitions
// startup_kickoff_state to given_up and emits a typed event with a
// registered payload.
func TestNudgeOutstandingStartupKickoffs_GivesUpAndEmitsTypedEventAtCap(t *testing.T) {
	sp := runningStartupKickoffFake(t)
	cfg := startupKickoffTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := namedDirectSessionBead("step-a", base)
	session.Metadata[startupKickoffStateKey] = startupKickoffStateNudged
	session.Metadata[startupKickoffAttemptsKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	session.Metadata[startupKickoffLastNudgeAtKey] = base.Format(time.RFC3339)
	boundSteps := []beads.Bead{{ID: "step-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	rec := events.NewFake()
	now := base.Add(2 * idleClaimNudgeBackoff)
	var out bytes.Buffer

	nudgeOutstandingStartupKickoffs(sp, cfg, store, []beads.Bead{session}, boundSteps, now, rec, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls at exhaustion = %d, want 0 (giving up is not another delivery attempt)", got)
	}

	session = mustGetStartupKickoffBead(t, store, session.ID)
	if got := session.Metadata[startupKickoffStateKey]; got != startupKickoffStateGivenUp {
		t.Fatalf("startup kickoff state = %q, want %q", got, startupKickoffStateGivenUp)
	}
	if got := session.Metadata[startupKickoffLastNudgeAtKey]; got != "" {
		t.Fatalf("startup kickoff last nudge at = %q, want cleared", got)
	}

	var givenUp []events.Event
	for _, e := range rec.Events {
		if e.Type == wantStartupKickoffGivenUpEventType {
			givenUp = append(givenUp, e)
		}
	}
	if len(givenUp) != 1 {
		t.Fatalf("session.startup_kickoff_given_up events = %d, want exactly 1", len(givenUp))
	}
	if givenUp[0].Subject != "step-a" || givenUp[0].SessionID != session.ID {
		t.Fatalf("given-up event subject=%q sessionID=%q, want subject=%q sessionID=%q",
			givenUp[0].Subject, givenUp[0].SessionID, "step-a", session.ID)
	}

	var payload startupKickoffGivenUpPayload
	if err := json.Unmarshal(givenUp[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshaling given-up payload: %v", err)
	}
	if payload.SessionID != session.ID || payload.BoundStepID != "step-a" || payload.Attempts != idleClaimNudgeMaxAttempts {
		t.Fatalf("given-up payload = %+v, want session %s step-a attempts %d", payload, session.ID, idleClaimNudgeMaxAttempts)
	}
}

// AC4 + AC6: once given up, the backstop must latch — it never re-nudges and
// never re-emits the give-up event on subsequent ticks (no infinite retry).
func TestNudgeOutstandingStartupKickoffs_NeverRenudgesAfterGivenUp(t *testing.T) {
	sp := runningStartupKickoffFake(t)
	cfg := startupKickoffTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := namedDirectSessionBead("step-a", base)
	session.Metadata[startupKickoffStateKey] = startupKickoffStateGivenUp
	session.Metadata[startupKickoffAttemptsKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	boundSteps := []beads.Bead{{ID: "step-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	rec := events.NewFake()
	now := base
	var out bytes.Buffer

	for range 5 {
		now = now.Add(idleClaimNudgeBackoff)
		session = mustGetStartupKickoffBead(t, store, session.ID)
		nudgeOutstandingStartupKickoffs(sp, cfg, store, []beads.Bead{session}, boundSteps, now, rec, &out)
	}

	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls after giving up = %d, want 0 across further ticks", got)
	}
	var givenUp int
	for _, e := range rec.Events {
		if e.Type == wantStartupKickoffGivenUpEventType {
			givenUp++
		}
	}
	if givenUp != 0 {
		t.Fatalf("re-emitted given_up events on an already-given-up session = %d, want 0 (must latch, not re-fire every tick)", givenUp)
	}
}

// AC5: pool-managed sessions are excluded from the named/direct path so they
// cannot be double-nudged by both mechanisms.
func TestNudgeOutstandingStartupKickoffs_ExcludesPoolManagedSessions(t *testing.T) {
	sp := runningStartupKickoffFake(t)
	cfg := startupKickoffTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := namedDirectSessionBead("step-a", base)
	session.Metadata["pool_managed"] = "true"
	boundSteps := []beads.Bead{{ID: "step-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	rec := events.NewFake()
	now := base.Add(24 * time.Hour)
	var out bytes.Buffer

	nudgeOutstandingStartupKickoffs(sp, cfg, store, []beads.Bead{session}, boundSteps, now, rec, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls for a pool-managed session = %d, want 0; pool backstops own delivery there, not this one", got)
	}

	session = mustGetStartupKickoffBead(t, store, session.ID)
	if got := session.Metadata[startupKickoffStateKey]; got != startupKickoffStatePending {
		t.Fatalf("startup kickoff state for a pool-managed session = %q, want untouched %q", got, startupKickoffStatePending)
	}
	if len(rec.Events) != 0 {
		t.Fatalf("recorded events for a pool-managed session = %d, want 0", len(rec.Events))
	}
}

// Cross-cycle correctness: a rebind onto a new bound step (per
// startupKickoffReopenMetadata) resets state/attempts/started_at but can
// still be carrying a stale last-nudge timestamp from the prior cycle. Grace
// must be measured from the fresh cycle, not the stale timestamp.
func TestNudgeOutstandingStartupKickoffs_FreshBindIgnoresStaleLastNudgeTimestamp(t *testing.T) {
	sp := runningStartupKickoffFake(t)
	cfg := startupKickoffTestCfg()
	staleNudgeAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rebindAt := staleNudgeAt.Add(48 * time.Hour)
	session := namedDirectSessionBead("step-b", rebindAt)
	session.Metadata[startupKickoffLastNudgeAtKey] = staleNudgeAt.Format(time.RFC3339)
	boundSteps := []beads.Bead{{ID: "step-b", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	rec := events.NewFake()
	var out bytes.Buffer

	nudgeOutstandingStartupKickoffs(sp, cfg, store, []beads.Bead{session}, boundSteps, rebindAt, rec, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls immediately after a fresh rebind = %d, want 0; grace must be measured from the new cycle, not a stale last-nudge timestamp carried over from the previous one", got)
	}
}
