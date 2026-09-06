package main

import (
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

	// checkNoAssignedWorkIdle drives a second, provider-agnostic idle clock
	// for pool sessions: continuous time during which the session holds no
	// open or in_progress assigned work. hasWork is this tick's assigned-work
	// observation (callers fail closed and pass true on a store error). The
	// first no-work observation anchors; a work observation clears the anchor;
	// the clock fires once the anchor is older than the session's timeout and
	// clears it so a replacement session re-measures from scratch. Resolves the
	// timeout exactly as checkIdle does (per-name, then template fallback).
	//
	// Motivation: the activity clock measures pane writes, and some agent TUIs
	// (pi) repaint once a second while idle, so it never ages for them and the
	// configured idle_timeout is structurally unenforceable. Assigned work is
	// the pool's own ground truth of "busy", so it is measured directly.
	checkNoAssignedWorkIdle(sessionName, template string, hasWork bool, now time.Time) bool

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
	noWorkSince                map[string]time.Time     // session name → start of the current continuous no-assigned-work run
}

// newIdleTracker creates an idle tracker. Returns nil if disabled.
// Callers check for nil before using.
func newIdleTracker() *memoryIdleTracker {
	return &memoryIdleTracker{
		timeouts:                   make(map[string]time.Duration),
		templateTimeouts:           make(map[string]time.Duration),
		templateFallbackExemptions: make(map[string]bool),
		noWorkSince:                make(map[string]time.Time),
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

// resolveTimeout returns the effective idle timeout for a session: its
// per-name registration first, then the template fallback unless the session
// is exempt. ok is false when no positive timeout applies.
func (m *memoryIdleTracker) resolveTimeout(sessionName, template string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	timeout, ok := m.timeouts[sessionName]
	exempt := m.templateFallbackExemptions[sessionName]
	if !ok && !exempt && template != "" {
		timeout, ok = m.templateTimeouts[template]
	}
	return timeout, ok && timeout > 0
}

func (m *memoryIdleTracker) checkIdle(sessionName, template string, sp runtime.Provider, now time.Time) bool {
	timeout, ok := m.resolveTimeout(sessionName, template)
	if !ok {
		return false
	}
	lastActivity, err := workerSessionTargetLastActivityWithConfig("", nil, sp, nil, sessionName)
	if err != nil || lastActivity.IsZero() {
		return false
	}
	return now.Sub(lastActivity) > timeout
}

func (m *memoryIdleTracker) checkNoAssignedWorkIdle(sessionName, template string, hasWork bool, now time.Time) bool {
	timeout, ok := m.resolveTimeout(sessionName, template)
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if hasWork {
		delete(m.noWorkSince, sessionName)
		return false
	}
	anchor, ok := m.noWorkSince[sessionName]
	if !ok {
		m.noWorkSince[sessionName] = now
		return false
	}
	if now.Sub(anchor) > timeout {
		delete(m.noWorkSince, sessionName)
		return true
	}
	return false
}
