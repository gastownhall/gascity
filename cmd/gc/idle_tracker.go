package main

import (
	"log"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// idleTracker checks for agents that have been idle longer than their
// configured timeout. Nil means idle checking is disabled (backward
// compatible). Follows the same nil-guard pattern as crashTracker.
//
// Timeouts may be registered two ways:
//   - Per session name (setTimeout) for sessions whose runtime names are
//     stable and knowable at controller startup — mainly configured named
//     sessions like mayor.
//   - Per agent template (setTimeoutForTemplate) for ephemeral pool agents
//     whose runtime session names are bead-derived and minted as work is
//     slung. Static slot enumeration (worker-1, worker-2, ...) does not
//     match those names, so a per-name registration silently misses every
//     pool session.
//
// checkIdle resolves a timeout by checking the session name first and
// falling back to the template — preserving named-session behavior while
// also covering bead-derived pool session names.
type idleTracker interface {
	// checkIdle returns true if the agent has been idle longer than its
	// configured timeout. Queries sp.GetLastActivity(). template is the
	// agent's qualified template name and is used as a fallback lookup
	// when the session name is not registered directly (pool sessions).
	checkIdle(sessionName, template string, sp runtime.Provider, now time.Time) bool

	// setTimeout configures the idle timeout for a single session name.
	// Used for sessions whose runtime names are deterministic at startup
	// (configured named sessions). Duration of 0 clears the entry.
	setTimeout(sessionName string, timeout time.Duration)

	// setTimeoutForTemplate configures the idle timeout for every session
	// belonging to an agent template. Used for ephemeral pool agents whose
	// runtime session names carry per-instance bead IDs and cannot be
	// enumerated up front. Duration of 0 clears the entry.
	setTimeoutForTemplate(template string, timeout time.Duration)

	// exemptTemplateFallbackForSession prevents one stable session from
	// inheriting the template timeout. Used for mode="always" named sessions
	// that share a template with pool siblings.
	exemptTemplateFallbackForSession(sessionName string)
}

// memoryIdleTracker is the production implementation of idleTracker.
type memoryIdleTracker struct {
	mu                         sync.Mutex
	timeouts                   map[string]time.Duration // session name → idle timeout
	templateTimeouts           map[string]time.Duration // agent template → idle timeout
	templateFallbackExemptions map[string]bool          // session name → skip template fallback
	reportedUnenforceable      map[string]bool          // session name → already warned that idle_timeout cannot fire
}

// newIdleTracker creates an idle tracker. Returns nil if disabled.
// Callers check for nil before using.
func newIdleTracker() *memoryIdleTracker {
	return &memoryIdleTracker{
		timeouts:                   make(map[string]time.Duration),
		templateTimeouts:           make(map[string]time.Duration),
		templateFallbackExemptions: make(map[string]bool),
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

func (m *memoryIdleTracker) setTimeoutForTemplate(template string, timeout time.Duration) {
	if template == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if timeout <= 0 {
		delete(m.templateTimeouts, template)
		return
	}
	m.templateTimeouts[template] = timeout
}

func (m *memoryIdleTracker) exemptTemplateFallbackForSession(sessionName string) {
	if sessionName == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateFallbackExemptions[sessionName] = true
}

func (m *memoryIdleTracker) checkIdle(sessionName, template string, sp runtime.Provider, now time.Time) bool {
	m.mu.Lock()
	timeout, ok := m.timeouts[sessionName]
	exempt := m.templateFallbackExemptions[sessionName]
	if !ok && !exempt && template != "" {
		timeout, ok = m.templateTimeouts[template]
	}
	m.mu.Unlock()
	if !ok || timeout <= 0 {
		return false
	}
	// A provider that cannot report activity makes idle_timeout inert, and
	// silently: GetLastActivity returns the zero time with a NIL error, which is
	// indistinguishable here from "active just now". checkIdle then returns
	// false on every tick forever, DecideIdleTimeout is never entered, and the
	// consecutive-defer backstop it feeds can never fire either. A session that
	// stops mid-bead becomes immortal and keeps its claim (ga-7ye, root cause of
	// ga-0nf: six workers held beads for 3-5 hours with idle_timeout=5m set).
	//
	// The capability is declared honestly by every provider and was consulted by
	// nothing. Consult it here, and say so once per session — a configured
	// timeout that cannot fire is an operator-visible misconfiguration, not a
	// detail to swallow.
	if sp != nil && !sp.Capabilities().CanReportActivity {
		m.reportUnenforceable(sessionName, "provider cannot report activity", timeout)
		return false
	}
	lastActivity, err := workerSessionTargetLastActivityWithConfig("", nil, sp, nil, sessionName)
	if err != nil {
		m.reportUnenforceable(sessionName, "last-activity lookup failed: "+err.Error(), timeout)
		return false
	}
	if lastActivity.IsZero() {
		m.reportUnenforceable(sessionName, "provider reported no activity timestamp", timeout)
		return false
	}
	return now.Sub(lastActivity) > timeout
}

// reportUnenforceable logs, once per session, that a configured idle_timeout
// cannot be evaluated. Once rather than per tick: the reconciler runs this on
// every session every cycle, so an unconditional log would bury the fleet in
// the same line and get filtered out, which is how it stayed invisible.
func (m *memoryIdleTracker) reportUnenforceable(sessionName, why string, timeout time.Duration) {
	m.mu.Lock()
	if m.reportedUnenforceable == nil {
		m.reportedUnenforceable = make(map[string]bool)
	}
	already := m.reportedUnenforceable[sessionName]
	m.reportedUnenforceable[sessionName] = true
	m.mu.Unlock()
	if already {
		return
	}
	log.Printf("idle_timeout=%s configured for session %q but cannot be enforced: %s. "+
		"The session will never be idle-stopped, and a session that stops mid-bead will hold its claim indefinitely. See ga-7ye.",
		timeout, sessionName, why)
}
