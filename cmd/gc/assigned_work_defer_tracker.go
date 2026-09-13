package main

import "sync"

// defaultAssignedWorkDeferLimit is the consecutive same-anchor assigned-work
// defer limit applied when neither a session nor its template has an
// explicit config.Agent.AssignedWorkDeferLimit override. ga-4tu2z7 suggested
// 3 as a reasonable starting point; the exact number is not load-bearing —
// only that some finite default exists so the backstop (ga-nllza6) is live
// out of the box instead of requiring every agent to opt in.
const defaultAssignedWorkDeferLimit = 3

// assignedWorkDeferTracker records, per session name, the number of
// consecutive idle-timeout ticks the reconciler has deferred specifically
// because DecideIdleTimeout found AssignedWorkHas on the same anchor bead.
// Nil means the backstop is disabled (same nil-guard convention as
// idleTracker/maxSessionAgeTracker): the reconciler skips recordDefer
// entirely and DecideIdleTimeout's ordinary AssignedWorkHas defer applies
// with no consecutive-defer limit.
//
// Limits may be registered two ways, mirroring idleTracker:
//   - Per session name (setLimit) for sessions whose runtime names are
//     stable and knowable at controller startup.
//   - Per agent template (setLimitForTemplate) for ephemeral pool agents
//     whose runtime session names are bead-derived and minted as work is
//     slung.
//
// Unlike idleTracker/maxSessionAgeTracker, an unregistered session is not
// treated as "feature off": recordDefer falls back to
// defaultAssignedWorkDeferLimit so the backstop stays live even for a
// session nobody explicitly configured. That is the deliberate divergence
// from both siblings' "unconfigured means off" convention.
type assignedWorkDeferTracker interface {
	// recordDefer records one more assigned-work idle-timeout defer for
	// sessionName anchored on anchorBeadID, and reports whether the
	// consecutive-defer count now exceeds the resolved limit (direct
	// session config, else template config unless exempt, else
	// defaultAssignedWorkDeferLimit). When anchorBeadID differs from the
	// session's previously recorded anchor — including first sight — the
	// count resets to zero before this defer is counted, so a fresh anchor
	// bead always starts at one.
	recordDefer(sessionName, template, anchorBeadID string) (exhausted bool)

	// reset clears sessionName's consecutive-defer count and remembered
	// anchor. Callers reset whenever the session is not idle-kill-eligible
	// on a tick — i.e. whenever the tick's idle-timeout outcome was not
	// itself an assigned-work defer (blocker, pending, no timer trigger, or
	// an ordinary AssignedWorkNone stop) — so a streak broken by any other
	// tick outcome does not bleed into a later, unrelated defer streak.
	reset(sessionName string)

	// setLimit configures a session's consecutive-defer limit. A limit of
	// 0 or less removes the session's direct override (falls back to
	// template config, then defaultAssignedWorkDeferLimit).
	setLimit(sessionName string, limit int)

	// setLimitForTemplate configures the consecutive-defer limit for every
	// session belonging to an agent template whose concrete runtime names
	// are minted after controller startup. A limit of 0 or less removes the
	// template override.
	setLimitForTemplate(template string, limit int)

	// exemptTemplateFallbackForSession prevents one stable session from
	// inheriting the template limit (falls back straight to
	// defaultAssignedWorkDeferLimit instead). Used for mode="always" named
	// sessions that share a template with pool siblings.
	exemptTemplateFallbackForSession(sessionName string)
}

// assignedWorkDeferState is one session's current consecutive-defer streak.
type assignedWorkDeferState struct {
	anchorBeadID string
	count        int
}

// memoryAssignedWorkDeferTracker is the production implementation of
// assignedWorkDeferTracker.
type memoryAssignedWorkDeferTracker struct {
	mu                         sync.Mutex
	limits                     map[string]int                    // session name → configured limit
	templateLimits             map[string]int                    // agent template → configured limit
	templateFallbackExemptions map[string]bool                   // session name → skip template fallback
	state                      map[string]assignedWorkDeferState // session name → current streak
}

// newAssignedWorkDeferTracker creates an assigned-work defer tracker.
func newAssignedWorkDeferTracker() *memoryAssignedWorkDeferTracker {
	return &memoryAssignedWorkDeferTracker{
		limits:                     make(map[string]int),
		templateLimits:             make(map[string]int),
		templateFallbackExemptions: make(map[string]bool),
		state:                      make(map[string]assignedWorkDeferState),
	}
}

func (m *memoryAssignedWorkDeferTracker) setLimit(sessionName string, limit int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		delete(m.limits, sessionName)
		return
	}
	m.limits[sessionName] = limit
}

func (m *memoryAssignedWorkDeferTracker) setLimitForTemplate(template string, limit int) {
	if template == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		delete(m.templateLimits, template)
		return
	}
	m.templateLimits[template] = limit
}

func (m *memoryAssignedWorkDeferTracker) exemptTemplateFallbackForSession(sessionName string) {
	if sessionName == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateFallbackExemptions[sessionName] = true
}

// limitFor resolves sessionName's consecutive-defer limit. Callers must hold
// m.mu.
func (m *memoryAssignedWorkDeferTracker) limitFor(sessionName, template string) int {
	if limit, ok := m.limits[sessionName]; ok {
		return limit
	}
	if !m.templateFallbackExemptions[sessionName] && template != "" {
		if limit, ok := m.templateLimits[template]; ok {
			return limit
		}
	}
	return defaultAssignedWorkDeferLimit
}

func (m *memoryAssignedWorkDeferTracker) recordDefer(sessionName, template, anchorBeadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[sessionName]
	if st.anchorBeadID != anchorBeadID {
		st = assignedWorkDeferState{anchorBeadID: anchorBeadID}
	}
	st.count++
	m.state[sessionName] = st
	return st.count > m.limitFor(sessionName, template)
}

func (m *memoryAssignedWorkDeferTracker) reset(sessionName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.state, sessionName)
}
