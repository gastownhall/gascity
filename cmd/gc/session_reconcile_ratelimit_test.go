// This file pins the desired post-fix behavior for rate-limit-blind respawns.

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

// TestCheckStability_RateLimitScreen_DoesNotCountAsCrash pins the desired
// post-fix behavior of checkStability when the agent's pane shows a
// Claude/Gemini rate-limit screen.
//
// When an agent CLI exits at the rate-limit screen, the session reconciler
// sees process_alive==false, calls checkStability, which sees last_woke_at
// within stabilityThreshold and counts it as a crash via recordWakeFailure.
// Five consecutive rate-limit exits within 30s trigger a 5-minute quarantine,
// so the system burns 5 wake/prime/--resume cycles before backing off, even
// though every wake will hit the same rate limit and produce zero useful work.
//
// Fix: extend checkStability to accept a peek callback (matching the shape
// already used by AcceptStartupDialogs* in internal/runtime/dialog.go). When
// peek returns high-confidence provider rate-limit screen content, the
// function records a rate-limit quarantine (longer back-off, distinct
// sleep_reason="rate_limit") instead of a crash, and does NOT increment
// wake_attempts.
func TestCheckStability_RateLimitScreen_DoesNotCountAsCrash(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at":        now.Add(-10 * time.Second).Format(time.RFC3339),
		"session_key":         "keep-session",
		"started_config_hash": "keep-hash",
		"wake_attempts":       "3", // a real crash would push us to 4
	})

	paneContent := "You've hit your limit, Pro plan\n\n/rate-limit-options"
	var gotLines int
	peek := func(lines int) (string, error) {
		gotLines = lines
		return paneContent, nil
	}

	if !checkStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek) {
		t.Fatal("checkStability should return true when it records a rate-limit hold")
	}

	if got := session.Metadata["wake_attempts"]; got != "3" {
		t.Errorf("wake_attempts = %q, want 3; rate-limit exit must not count as a crash", got)
	}

	if got := session.Metadata["sleep_reason"]; got != "rate_limit" {
		t.Errorf("sleep_reason = %q, want %q", got, "rate_limit")
	}
	if got := session.Metadata["state"]; got != "asleep" {
		t.Errorf("state = %q, want asleep", got)
	}

	qUntil, err := time.Parse(time.RFC3339, session.Metadata["quarantined_until"])
	if err != nil {
		t.Fatalf("quarantined_until parse: %v", err)
	}
	if want := now.Add(defaultRateLimitQuarantineDuration); !qUntil.Equal(want) {
		t.Errorf("quarantined_until = %s, want %s", qUntil.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	if gotLines != rateLimitPeekLines {
		t.Errorf("peek lines = %d, want %d", gotLines, rateLimitPeekLines)
	}

	if got := session.Metadata["session_key"]; got != "keep-session" {
		t.Errorf("session_key = %q, want preserved", got)
	}
	if got := session.Metadata["started_config_hash"]; got != "keep-hash" {
		t.Errorf("started_config_hash = %q, want preserved", got)
	}

	// last_woke_at should be cleared (edge-triggered, mirroring the existing
	// crash path) so the rate-limit detection isn't re-triggered next tick.
	if session.Metadata["last_woke_at"] != "" {
		t.Error("last_woke_at should be cleared after rate-limit detection")
	}
}

func TestCheckStability_RateLimitPendingCreateClearsStartedAt(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at":              now.Add(-2 * time.Minute).Format(time.RFC3339),
		"session_key":               "keep-session",
		"started_config_hash":       "keep-hash",
		"wake_attempts":             "3",
		"pending_create_claim":      "true",
		"pending_create_started_at": now.Add(-20 * time.Second).Format(time.RFC3339),
	})

	peek := func(_ int) (string, error) {
		return "You've hit your limit, Pro plan\n\n/rate-limit-options", nil
	}

	if !checkStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek) {
		t.Fatal("checkStability should return true when it records a rate-limit hold")
	}
	if session.Metadata["pending_create_claim"] != "" {
		t.Error("pending_create_claim should be cleared after rate-limit detection")
	}
	if session.Metadata["pending_create_started_at"] != "" {
		t.Error("pending_create_started_at should be cleared with pending_create_claim")
	}
}

func TestCheckRateLimitStability_BeforeHealPreservesResumeMetadata(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"state":               "active",
		"last_woke_at":        now.Add(-10 * time.Second).Format(time.RFC3339),
		"session_key":         "keep-session",
		"started_config_hash": "keep-hash",
	})

	peek := func(_ int) (string, error) {
		return "You've hit your limit, Pro plan\n\n/rate-limit-options", nil
	}

	handled, err := checkRateLimitStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek)
	if err != nil {
		t.Fatalf("recording rate-limit rapid exit: %v", err)
	}
	if !handled {
		t.Fatal("rate-limit rapid exit should be recorded before advisory state healing")
	}

	healState(&session, false, sessionFrontDoor(store), clk)

	if got := session.Metadata["session_key"]; got != "keep-session" {
		t.Errorf("session_key = %q, want preserved", got)
	}
	if got := session.Metadata["started_config_hash"]; got != "keep-hash" {
		t.Errorf("started_config_hash = %q, want preserved", got)
	}
	if got := session.Metadata["continuation_reset_pending"]; got != "" {
		t.Errorf("continuation_reset_pending = %q, want empty", got)
	}
	if got := session.Metadata["state"]; got != "asleep" {
		t.Errorf("state = %q, want asleep", got)
	}
	if got := session.Metadata["sleep_reason"]; got != "rate_limit" {
		t.Errorf("sleep_reason = %q, want rate_limit", got)
	}
}

func TestCheckRateLimitStability_BatchFailureDoesNotClearLastWokeAt(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	store.metadataBatchErr = errors.New("batch failed")
	dt := newDrainTracker()
	lastWoke := now.Add(-10 * time.Second).Format(time.RFC3339)

	session := makeBead("b1", map[string]string{
		"state":               "active",
		"last_woke_at":        lastWoke,
		"session_key":         "keep-session",
		"started_config_hash": "keep-hash",
	})
	peek := func(_ int) (string, error) {
		return "You've hit your limit, Pro plan\n\n/rate-limit-options", nil
	}

	handled, err := checkRateLimitStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek)
	if err == nil {
		t.Fatal("rate-limit batch failure should be returned")
	}
	if handled {
		t.Fatal("rate-limit rapid exit should not be handled when persistence fails")
	}
	if got := session.Metadata["last_woke_at"]; got != lastWoke {
		t.Fatalf("last_woke_at = %q, want preserved after failed batch", got)
	}
	if got := session.Metadata["sleep_reason"]; got != "" {
		t.Fatalf("sleep_reason = %q, want unchanged after failed batch", got)
	}
	if got, ok := store.metadata["b1"]["last_woke_at"]; ok {
		t.Fatalf("separate last_woke_at write = %q, want no standalone clear", got)
	}
	if len(store.metadataBatchPatches) != 1 {
		t.Fatalf("metadata batch calls = %d, want 1", len(store.metadataBatchPatches))
	}
	if got, ok := store.metadataBatchPatches[0]["last_woke_at"]; !ok || got != "" {
		t.Fatalf("rate-limit batch last_woke_at = %q, present=%v; want empty value in batch", got, ok)
	}

	store.metadataBatchErr = nil
	handled, err = checkRateLimitStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek)
	if err != nil {
		t.Fatalf("retrying rate-limit detection: %v", err)
	}
	if !handled {
		t.Fatal("rate-limit detection should retry on the next tick after a failed batch")
	}
	healState(&session, false, sessionFrontDoor(store), clk)

	if got := session.Metadata["session_key"]; got != "keep-session" {
		t.Errorf("session_key = %q, want preserved", got)
	}
	if got := session.Metadata["started_config_hash"]; got != "keep-hash" {
		t.Errorf("started_config_hash = %q, want preserved", got)
	}
	if got := session.Metadata["continuation_reset_pending"]; got != "" {
		t.Errorf("continuation_reset_pending = %q, want empty", got)
	}
	if got := session.Metadata["last_woke_at"]; got != "" {
		t.Errorf("last_woke_at = %q, want cleared by successful quarantine batch", got)
	}
}

func TestCheckRateLimitStability_BatchFailureRetriesAfterStabilityThreshold(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	store.metadataBatchErr = errors.New("batch failed")
	dt := newDrainTracker()
	lastWoke := now.Add(-10 * time.Second).Format(time.RFC3339)

	session := makeBead("b1", map[string]string{
		"state":               "active",
		"last_woke_at":        lastWoke,
		"session_key":         "keep-session",
		"started_config_hash": "keep-hash",
	})
	peek := func(_ int) (string, error) {
		return "You've hit your limit, Pro plan\n\n/rate-limit-options", nil
	}

	handled, err := checkRateLimitStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek)
	if err == nil {
		t.Fatal("initial failed batch should be returned")
	}
	if handled {
		t.Fatal("initial failed batch should not be reported as handled")
	}

	clk.Time = now.Add(stabilityThreshold + time.Second)
	store.metadataBatchErr = nil
	handled, err = checkRateLimitStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek)
	if err != nil {
		t.Fatalf("retrying after stability threshold: %v", err)
	}
	if !handled {
		t.Fatal("rate-limit detection should retry after the crash stability threshold")
	}
	healState(&session, false, sessionFrontDoor(store), clk)

	if got := session.Metadata["session_key"]; got != "keep-session" {
		t.Errorf("session_key = %q, want preserved", got)
	}
	if got := session.Metadata["started_config_hash"]; got != "keep-hash" {
		t.Errorf("started_config_hash = %q, want preserved", got)
	}
	if got := session.Metadata["continuation_reset_pending"]; got != "" {
		t.Errorf("continuation_reset_pending = %q, want empty", got)
	}
	if got := session.Metadata["sleep_reason"]; got != "rate_limit" {
		t.Errorf("sleep_reason = %q, want rate_limit", got)
	}
}

// TestCheckStability_RateLimitScreen_EmptyPaneStillCountsAsCrash ensures the
// rate-limit detection requires positive evidence in the pane. If peek
// returns nothing matching the rate-limit signature, behavior matches the
// existing crash path: count as a crash, increment wake_attempts.
func TestCheckStability_RateLimitScreen_EmptyPaneStillCountsAsCrash(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at":  now.Add(-10 * time.Second).Format(time.RFC3339),
		"wake_attempts": "0",
	})

	peek := func(_ int) (string, error) { return "", nil }

	if !checkStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek) {
		t.Error("rapid exit with no rate-limit signature should report stability failure")
	}
	if got := session.Metadata["wake_attempts"]; got != "1" {
		t.Errorf("wake_attempts = %q, want 1", got)
	}
}

// TestCheckStability_RateLimitScreen_NilPeekFallsBackToCrash ensures
// backward compatibility for call sites that don't supply a peek (subprocess
// providers, test paths). When peek is nil, behavior matches the legacy
// crash-only path.
func TestCheckStability_RateLimitScreen_NilPeekFallsBackToCrash(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at":  now.Add(-10 * time.Second).Format(time.RFC3339),
		"wake_attempts": "0",
	})

	if !checkStability(&session, nil, false, dt, sessionFrontDoor(store), clk, nil) {
		t.Error("rapid exit with nil peek should fall back to crash-counting behavior")
	}
	if got := session.Metadata["wake_attempts"]; got != "1" {
		t.Errorf("wake_attempts = %q, want 1", got)
	}
}

func TestCheckStability_RateLimitScreen_PeekErrorFallsBackToCrash(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at":  now.Add(-10 * time.Second).Format(time.RFC3339),
		"wake_attempts": "0",
	})

	peek := func(_ int) (string, error) {
		return "", errors.New("peek failed")
	}

	if !checkStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek) {
		t.Error("rapid exit with peek error should fall back to crash-counting behavior")
	}
	if got := session.Metadata["wake_attempts"]; got != "1" {
		t.Errorf("wake_attempts = %q, want 1", got)
	}
}

// TestCheckStability_TerminalErrorScreen_MarksTerminalNotCrash pins fix-finding
// #2: a non-zombie dead session (running==false, so the reconciler's
// `running && !alive` zombie capture never fires) whose provider screen shows a
// terminal, non-retryable error must be classified terminal here — marked
// unhealthy + drainable so pool sizing excludes its slot — instead of being
// counted as an ordinary crash and retried forever.
func TestCheckStability_TerminalErrorScreen_MarksTerminalNotCrash(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at":  now.Add(-10 * time.Second).Format(time.RFC3339),
		"wake_attempts": "3", // a real crash would push us to 4
	})

	peek := func(_ int) (string, error) {
		return "model_not_found: gpt-5.3-codex-spark", nil
	}

	if !checkStability(&session, nil, false, dt, sessionFrontDoor(store), clk, peek) {
		t.Fatal("checkStability should return true when it records a terminal provider error")
	}
	if got := session.Metadata["wake_attempts"]; got != "3" {
		t.Errorf("wake_attempts = %q, want 3; a terminal provider error must not count as a crash", got)
	}
	if got := session.Metadata["state"]; got != "asleep" {
		t.Errorf("state = %q, want asleep", got)
	}
	if got := session.Metadata["sleep_reason"]; got != sleepReasonProviderTerminalError {
		t.Errorf("sleep_reason = %q, want %q", got, sleepReasonProviderTerminalError)
	}
	if got := session.Metadata[sessionProviderTerminalErrorMetadataKey]; got != "model_not_found" {
		t.Errorf("%s = %q, want model_not_found", sessionProviderTerminalErrorMetadataKey, got)
	}
	if got := session.Metadata[sessionHealthStateMetadataKey]; got != "unhealthy" {
		t.Errorf("%s = %q, want unhealthy", sessionHealthStateMetadataKey, got)
	}
	if got := session.Metadata[sessionDrainableMetadataKey]; got != boolMetadata(true) {
		t.Errorf("%s = %q, want %q", sessionDrainableMetadataKey, got, boolMetadata(true))
	}
	// Edge-triggered: last_woke_at cleared so the terminal classification isn't
	// re-evaluated (and can't accrue a wake failure) on the next tick.
	if got := session.Metadata["last_woke_at"]; got != "" {
		t.Errorf("last_woke_at = %q, want cleared after terminal classification", got)
	}
}
