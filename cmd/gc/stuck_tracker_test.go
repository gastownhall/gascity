package main

import (
	"crypto/sha256"
	"testing"
	"time"
)

func TestStuckTracker_NilSafe(t *testing.T) {
	var st stuckTracker // nil interface
	if st != nil {
		t.Fatal("nil stuckTracker should be nil")
	}
}

func TestStuckTracker_DisabledWhenTimeoutZero(t *testing.T) {
	st := newStuckTracker(0)
	if st != nil {
		t.Fatal("expected nil tracker for zero timeout")
	}
	st = newStuckTracker(-1 * time.Minute)
	if st != nil {
		t.Fatal("expected nil tracker for negative timeout")
	}
}

func TestStuckTracker_FirstSeen_GracePeriod(t *testing.T) {
	st := newStuckTracker(10 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// First call: establishes firstSeen, always returns false.
	if st.checkStuck("sess1", "some output", now) {
		t.Fatal("expected false on first observation")
	}

	// Same hash within grace period — still false.
	if st.checkStuck("sess1", "some output", now.Add(5*time.Minute)) {
		t.Fatal("expected false within grace period")
	}
}

func TestStuckTracker_HashChanges_ResetsTimer(t *testing.T) {
	st := newStuckTracker(5 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	st.checkStuck("sess1", "output1", now)
	// Wait past grace period, hash changes.
	st.checkStuck("sess1", "output2", now.Add(6*time.Minute))

	// Now same hash for < timeout — not stuck.
	if st.checkStuck("sess1", "output2", now.Add(10*time.Minute)) {
		t.Fatal("expected false: hash changed 4 minutes ago")
	}
}

func TestStuckTracker_HashStale_ReturnsTrue(t *testing.T) {
	st := newStuckTracker(5 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	st.checkStuck("sess1", "frozen output", now)

	// Past grace period + past timeout with unchanged hash.
	if !st.checkStuck("sess1", "frozen output", now.Add(11*time.Minute)) {
		t.Fatal("expected true: hash stale for > timeout past grace period")
	}
}

func TestStuckTracker_HashStale_JustUnder_ReturnsFalse(t *testing.T) {
	st := newStuckTracker(10 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	st.checkStuck("sess1", "frozen output", now)

	// Exactly at timeout (not past) — should NOT fire.
	if st.checkStuck("sess1", "frozen output", now.Add(10*time.Minute)) {
		t.Fatal("expected false: exactly at timeout, not past it")
	}
}

func TestStuckTracker_EmptyOutput_SkipsSession(t *testing.T) {
	st := newStuckTracker(5 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Empty output is skipped (per-session not-peekable signal).
	if st.checkStuck("sess1", "", now) {
		t.Fatal("expected false for empty output")
	}
	if st.checkStuck("sess1", "", now.Add(1*time.Hour)) {
		t.Fatal("expected false for empty output even after long time")
	}
}

func TestStuckTracker_ClearSession_ResetsState(t *testing.T) {
	st := newStuckTracker(5 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	st.checkStuck("sess1", "output", now)
	st.clearSession("sess1")

	// After clear, next call is a fresh first observation.
	if st.checkStuck("sess1", "output", now.Add(1*time.Hour)) {
		t.Fatal("expected false: clearSession should reset state")
	}
}

func TestStuckTracker_MultipleSessionsIndependent(t *testing.T) {
	st := newStuckTracker(5 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	st.checkStuck("sess1", "output A", now)
	st.checkStuck("sess2", "output B", now)

	// sess1 stays frozen, sess2 changes.
	st.checkStuck("sess1", "output A", now.Add(11*time.Minute))
	st.checkStuck("sess2", "output C", now.Add(11*time.Minute))

	if !st.checkStuck("sess1", "output A", now.Add(12*time.Minute)) {
		t.Fatal("expected sess1 stuck")
	}
	if st.checkStuck("sess2", "output C", now.Add(12*time.Minute)) {
		t.Fatal("expected sess2 not stuck — hash changed recently")
	}
}

func TestStuckTracker_OwnCircuitBreaker_Quarantines(t *testing.T) {
	st := newStuckTracker(10 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Initialize session.
	st.checkStuck("sess1", "output", now)

	// Record kills within the window (2 * timeout = 20 minutes).
	if st.recordKill("sess1", now.Add(1*time.Minute)) {
		t.Fatal("expected no quarantine after 1 kill")
	}
	if st.recordKill("sess1", now.Add(5*time.Minute)) {
		t.Fatal("expected no quarantine after 2 kills")
	}
	if !st.recordKill("sess1", now.Add(10*time.Minute)) {
		t.Fatal("expected quarantine after 3 kills within window")
	}
}

func TestStuckTracker_OwnCircuitBreaker_WindowExpiry(t *testing.T) {
	st := newStuckTracker(10 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	st.checkStuck("sess1", "output", now)

	// Two kills at t=0.
	st.recordKill("sess1", now)
	st.recordKill("sess1", now.Add(1*time.Minute))

	// Third kill well outside the window (2*10m = 20m). Old kills pruned.
	if st.recordKill("sess1", now.Add(30*time.Minute)) {
		t.Fatal("expected no quarantine: old kills outside window")
	}
}

func TestDoCheckStuck_PureFunction(t *testing.T) {
	timeout := 5 * time.Minute
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("hash_changes", func(t *testing.T) {
		entry := stuckEntry{
			hash:      sha256.Sum256([]byte("old output")),
			changedAt: now,
			firstSeen: now.Add(-10 * time.Minute),
		}
		stuck, updated := doCheckStuck(entry, "new output", now.Add(6*time.Minute), timeout)
		if stuck {
			t.Fatal("expected false when hash changes")
		}
		if updated.hash == entry.hash {
			t.Fatal("expected hash to be updated")
		}
	})

	t.Run("stale_hash_triggers", func(t *testing.T) {
		entry := stuckEntry{
			hash:      sha256.Sum256([]byte("frozen")),
			changedAt: now,
			firstSeen: now.Add(-10 * time.Minute),
		}
		stuck, _ := doCheckStuck(entry, "frozen", now.Add(6*time.Minute), timeout)
		if !stuck {
			t.Fatal("expected true: hash stale for > timeout")
		}
	})

	t.Run("grace_period_suppresses", func(t *testing.T) {
		entry := stuckEntry{
			hash:      sha256.Sum256([]byte("frozen")),
			changedAt: now,
			firstSeen: now,
		}
		stuck, _ := doCheckStuck(entry, "frozen", now.Add(3*time.Minute), timeout)
		if stuck {
			t.Fatal("expected false: within grace period")
		}
	})
}

func TestTruncatePeekSnippet(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"empty", "", 120, ""},
		{"short", "hello", 120, "hello"},
		{"newline", "line1\nline2", 120, "line1"},
		{"long", "abcdefghij", 5, "abcde"},
		{"newline_before_max", "ab\ncd", 10, "ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncatePeekSnippet(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncatePeekSnippet(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}
