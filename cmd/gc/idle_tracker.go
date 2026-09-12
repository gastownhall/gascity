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
	// configured timeout. template is the agent's qualified template name and
	// is used as a fallback lookup when the session name is not registered
	// directly (pool sessions). provider and transport identify the runtime so
	// the tracker can pick the right idle measurement: interactive TUIs whose
	// coarse pane-activity clock cannot see idleness (the Claude Code TUI over
	// a non-ACP transport) are measured by a content-based pane scan, all
	// others by sp.GetLastActivity().
	checkIdle(sessionName, template, provider, transport string, sp runtime.Provider, now time.Time) bool

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
	idleSince                  map[string]time.Time     // session name → start of the current continuous content-idle run
}

// newIdleTracker creates an idle tracker. Returns nil if disabled.
// Callers check for nil before using.
func newIdleTracker() *memoryIdleTracker {
	return &memoryIdleTracker{
		timeouts:                   make(map[string]time.Duration),
		templateTimeouts:           make(map[string]time.Duration),
		templateFallbackExemptions: make(map[string]bool),
		idleSince:                  make(map[string]time.Time),
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

func (m *memoryIdleTracker) checkIdle(sessionName, template, provider, transport string, sp runtime.Provider, now time.Time) bool {
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

	// Interactive TUIs whose coarse pane-activity clock cannot distinguish an
	// idle-but-repainting session from a working one need a content-based idle
	// measurement. The Claude Code TUI over a non-ACP transport continuously
	// repaints its status line, so GetLastActivity stays perpetually fresh and
	// the activity-clock test below never fires — leaving an idle always-on
	// heartbeat awake indefinitely (ga-07mi8). For those sessions, drive an
	// idle-since anchor from a point-in-time pane scan instead, when the
	// runtime can take one.
	if idleTrackerContentClockApplies(provider, transport) {
		if snap, ok := sp.(runtime.IdleSnapshotProvider); ok {
			return m.checkIdleByContent(sessionName, snap, timeout, now)
		}
	}

	lastActivity, err := workerSessionTargetLastActivityWithConfig("", nil, sp, nil, sessionName)
	if err != nil || lastActivity.IsZero() {
		return false
	}
	return now.Sub(lastActivity) > timeout
}

// checkIdleByContent drives the per-session idle-since anchor from a
// point-in-time pane scan and reports whether the session has been
// continuously idle longer than timeout. A busy observation resets the anchor;
// firing clears it so a restarted session re-measures from scratch. An
// observation error leaves the anchor untouched — a transient read failure
// must neither reset a long accumulation nor be mistaken for idleness.
func (m *memoryIdleTracker) checkIdleByContent(sessionName string, snap runtime.IdleSnapshotProvider, timeout time.Duration, now time.Time) bool {
	idle, err := snap.SnapshotIdle(sessionName)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !idle {
		delete(m.idleSince, sessionName)
		return false
	}
	anchor, ok := m.idleSince[sessionName]
	if !ok {
		m.idleSince[sessionName] = now
		return false
	}
	if now.Sub(anchor) > timeout {
		delete(m.idleSince, sessionName)
		return true
	}
	return false
}

// idleTrackerContentClockApplies reports whether a session needs the
// content-based idle clock instead of the coarse pane-activity clock: the
// Claude Code TUI over a non-ACP transport. ACP delivers in-process (no
// repainting pane) and non-Claude providers do not hold a continuously
// repainting interactive composer, so their activity clock reflects real
// idleness. This mirrors pollerTUIComposerCanStick on the nudge path
// (ga-32mpo).
func idleTrackerContentClockApplies(provider, transport string) bool {
	return transport != "acp" && provider == "claude"
}
