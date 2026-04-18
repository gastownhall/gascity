package main

import (
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// idleTracker checks for agents that have been idle longer than their
// configured timeout. Nil means idle checking is disabled (backward
// compatible). Follows the same nil-guard pattern as crashTracker.
type idleTracker interface {
	// checkIdle returns true if the agent has been idle longer than its
	// configured timeout. Queries sp.GetLastActivity().
	checkIdle(sessionName string, sp runtime.Provider, now time.Time) bool

	// setTimeout configures the idle timeout for a session name.
	// Called during agent list construction. Duration of 0 disables.
	setTimeout(sessionName string, timeout time.Duration)
}

// memoryIdleTracker is the production implementation of idleTracker.
type memoryIdleTracker struct {
	mu       sync.Mutex
	timeouts map[string]time.Duration // session → idle timeout
}

// newIdleTracker creates an idle tracker. Returns nil if disabled.
// Callers check for nil before using.
func newIdleTracker() *memoryIdleTracker {
	return &memoryIdleTracker{
		timeouts: make(map[string]time.Duration),
	}
}

func (m *memoryIdleTracker) setTimeout(sessionName string, timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if timeout <= 0 {
		delete(m.timeouts, sessionName)
		return
	}
	m.timeouts[sessionName] = timeout
}

func (m *memoryIdleTracker) checkIdle(sessionName string, sp runtime.Provider, now time.Time) bool {
	m.mu.Lock()
	timeout, ok := m.timeouts[sessionName]
	m.mu.Unlock()
	if !ok || timeout <= 0 {
		return false
	}
	lastActivity, err := workerSessionTargetLastActivityWithConfig("", nil, sp, nil, sessionName)
	if err != nil || lastActivity.IsZero() {
		return false
	}
	return now.Sub(lastActivity) > timeout
}
