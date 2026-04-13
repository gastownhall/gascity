package main

import (
	"crypto/sha256"
	"sync"
	"time"
)

// stuckPeekLines is the number of terminal lines captured for stuck
// detection. Not configurable — 50 lines is sufficient for hash
// differentiation without excessive capture overhead.
const stuckPeekLines = 50

// stuckKillsMax is the maximum number of stuck-kills within the
// quarantine window before the session is quarantined. Prevents
// infinite kill-restart loops when the underlying cause persists.
const stuckKillsMax = 3

// stuckTracker detects sessions whose terminal output has not changed
// for longer than a configured timeout. Nil means stuck detection is
// disabled (backward compatible). Follows the same nil-guard pattern
// as crashTracker and idleTracker.
type stuckTracker interface {
	// checkStuck returns true if the session's output hash has been
	// unchanged for longer than the timeout. The output parameter is
	// the result of sp.Peek(name, stuckPeekLines). Empty output with
	// no error means the session is not peekable (skip).
	checkStuck(sessionName string, output string, now time.Time) bool

	// recordKill notes that a session was killed for being stuck.
	// Returns true if the session should be quarantined (exceeded
	// stuckKillsMax kills within the quarantine window).
	recordKill(sessionName string, now time.Time) bool

	// clearSession removes all tracking for a session. Called when a
	// session is stopped so the restarted session gets a fresh grace
	// period and kill window.
	clearSession(sessionName string)

	// timeout returns the configured stuck timeout duration.
	timeout() time.Duration
}

// stuckEntry holds per-session state for stuck detection.
type stuckEntry struct {
	hash      [32]byte
	changedAt time.Time
	firstSeen time.Time
	kills     []time.Time // recent stuck-kills for own circuit breaker
}

// memoryStuckTracker is the production implementation of stuckTracker.
// State is in-memory only — intentionally lost on controller restart
// (same as crashTracker, Erlang/OTP model).
type memoryStuckTracker struct {
	mu        sync.Mutex
	stuckTime time.Duration
	sessions  map[string]*stuckEntry
}

// newStuckTracker creates a stuck tracker with the given timeout.
// Returns nil if timeout <= 0 (disabled). Callers check for nil
// before using, same pattern as crashTracker and idleTracker.
func newStuckTracker(timeout time.Duration) stuckTracker {
	if timeout <= 0 {
		return nil
	}
	return &memoryStuckTracker{
		stuckTime: timeout,
		sessions:  make(map[string]*stuckEntry),
	}
}

func (m *memoryStuckTracker) timeout() time.Duration {
	return m.stuckTime
}

func (m *memoryStuckTracker) checkStuck(sessionName string, output string, now time.Time) bool {
	// Empty output means this session is not peekable (per-session
	// fallback for hybrid/auto providers). Skip without updating state.
	if output == "" {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.sessions[sessionName]
	if !ok {
		// First observation — initialize with current hash and time.
		// The grace period starts from now.
		h := sha256.Sum256([]byte(output))
		m.sessions[sessionName] = &stuckEntry{
			hash:      h,
			changedAt: now,
			firstSeen: now,
		}
		return false
	}

	stuck, updated := doCheckStuck(*entry, output, now, m.stuckTime)
	*entry = updated
	return stuck
}

func (m *memoryStuckTracker) recordKill(sessionName string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.sessions[sessionName]
	if !ok {
		return false
	}

	// Prune kills outside the quarantine window (2 * timeout).
	window := 2 * m.stuckTime
	cutoff := now.Add(-window)
	pruned := entry.kills[:0]
	for _, t := range entry.kills {
		if !t.Before(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, now)
	entry.kills = pruned

	return len(entry.kills) >= stuckKillsMax
}

func (m *memoryStuckTracker) clearSession(sessionName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionName)
}

// doCheckStuck is the pure detection function. It compares the SHA-256
// hash of the current output against the stored hash. If the hash has
// not changed for longer than the timeout, the session is stuck.
//
// This function is independently testable without any Provider mock.
func doCheckStuck(entry stuckEntry, output string, now time.Time, timeout time.Duration) (stuck bool, updated stuckEntry) {
	updated = entry
	h := sha256.Sum256([]byte(output))

	if h != entry.hash {
		// Output changed — reset the staleness timer.
		updated.hash = h
		updated.changedAt = now
		return false, updated
	}

	// Hash unchanged. Check if we're still in the startup grace period.
	if now.Sub(entry.firstSeen) < timeout {
		return false, updated
	}

	// Hash has been unchanged long enough — session is stuck.
	return now.Sub(entry.changedAt) > timeout, updated
}

// truncatePeekSnippet clips output to the first line or maxLen chars,
// whichever comes first. Used for the event Message field.
func truncatePeekSnippet(output string, maxLen int) string {
	for i, ch := range output {
		if ch == '\n' || i >= maxLen {
			return output[:i]
		}
	}
	if len(output) > maxLen {
		return output[:maxLen]
	}
	return output
}
