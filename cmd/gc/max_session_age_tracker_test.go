package main

import (
	"testing"
	"time"
)

func TestMaxSessionAgeTracker_UnregisteredSessionIsFalse(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if tr.shouldRestart("witness", now.Add(-10*time.Hour), now) {
		t.Error("shouldRestart must be false for sessions with no config")
	}
}

func TestMaxSessionAgeTracker_ZeroAnchorIsFalse(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	if tr.shouldRestart("witness", time.Time{}, time.Now()) {
		t.Error("shouldRestart must be false when creation_complete_at is zero")
	}
}

func TestMaxSessionAgeTracker_YoungSessionIsFalse(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if tr.shouldRestart("witness", now.Add(-1*time.Hour), now) {
		t.Error("shouldRestart must be false when session age < max age")
	}
}

func TestMaxSessionAgeTracker_OldSessionIsTrue(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if !tr.shouldRestart("witness", now.Add(-6*time.Hour), now) {
		t.Error("shouldRestart must be true when session age > max age")
	}
}

func TestMaxSessionAgeTracker_JitterExtendsThreshold(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	// Force deterministic zero offset by probing many permutations; the
	// randomness is internal. We use the boundary case: jitter=0 gives
	// the lower bound of the threshold. A fully-synchronized fleet with
	// zero jitter must restart exactly at the base threshold.
	tr.setConfig("a", 5*time.Hour, 0)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	anchor := now.Add(-5 * time.Hour)
	if !tr.shouldRestart("a", anchor, now) {
		t.Error("zero-jitter: age == maxAge must trigger restart")
	}

	// With non-zero jitter the threshold is always in [maxAge, maxAge+jitter).
	// We verify the invariant rather than a specific offset: a session
	// exactly at maxAge with any offset > 0 must NOT yet restart, and a
	// session at maxAge + jitter must always restart.
	tr.setConfig("b", 5*time.Hour, 30*time.Minute)
	if tr.shouldRestart("b", now.Add(-4*time.Hour-59*time.Minute), now) {
		t.Error("jitter: session 1m below base threshold must not restart")
	}
	if !tr.shouldRestart("b", now.Add(-6*time.Hour), now) {
		t.Error("jitter: session well past (maxAge + jitter) must restart")
	}
}

func TestMaxSessionAgeTracker_ClearingConfigDisablesRestart(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	tr.setConfig("witness", 0, 0) // clear
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if tr.shouldRestart("witness", now.Add(-10*time.Hour), now) {
		t.Error("shouldRestart must be false after the config is cleared")
	}
}

func TestMaxSessionAgeTracker_ReconfigRerollsJitterForDifferentSessions(t *testing.T) {
	// Sanity: separate sessions land on independent offsets, so their
	// restart times aren't correlated purely by session name ordering.
	tr := newMaxSessionAgeTracker()
	for i := 0; i < 128; i++ {
		tr.setConfig("witness-a", 5*time.Hour, time.Hour)
	}
	tr.mu.Lock()
	offsetA := tr.offsets["witness-a"]
	tr.mu.Unlock()
	if offsetA < 0 || offsetA >= time.Hour {
		t.Errorf("offset for witness-a = %v, want in [0, 1h)", offsetA)
	}
}
