package main

import (
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/google/uuid"
)

// sessionLivenessTracker monitors whether named sessions have had recent
// activity within a configured freshness window. It fires an escalation
// callback exactly once per stale episode (fresh→stale transition) and
// resets when the session becomes active again (stale→fresh).
//
// This provides the controller-supervised, tight-cadence patrol described
// in the health monitoring architecture: instead of relying on a pack
// formula's self-scheduled ScheduleWakeup (which can fail silently), the
// controller checks freshness on every patrol tick and escalates once per
// stale episode.
//
// Nil means the feature is disabled (no checks configured). Callers check
// for nil before using, following the same nil-guard pattern as idleTracker.
type sessionLivenessTracker interface {
	// checkFreshness checks whether sessionName's last activity is within
	// window. If the session is stale and no alert has been sent for the
	// current episode, emitStale is called exactly once. Returns true if the
	// session is stale, false if fresh. A zero LastActivity time is treated as
	// stale once window has elapsed since the controller started (startedAt).
	checkFreshness(
		sessionName string,
		window time.Duration,
		sp runtime.Provider,
		now time.Time,
		startedAt time.Time,
		emitStale func(episodeID string, staleSince time.Time, lastActivity time.Time),
	) bool

	// recordFreshTick clears the stale episode for sessionName. Call when
	// checkFreshness determines the session is fresh so the next stale
	// transition opens a new episode and fires a new alert.
	recordFreshTick(sessionName string)
}

// sessionEpisodeState tracks one session's stale/fresh episode.
type sessionEpisodeState struct {
	stale      bool
	alertSent  bool
	episodeID  string
	staleSince time.Time
}

// memorySessionLivenessTracker is the production implementation.
type memorySessionLivenessTracker struct {
	mu       sync.Mutex
	episodes map[string]*sessionEpisodeState
}

// newSessionLivenessTracker allocates a tracker. Returns nil if checks is empty.
func newSessionLivenessTracker(hasAny bool) *memorySessionLivenessTracker {
	if !hasAny {
		return nil
	}
	return &memorySessionLivenessTracker{
		episodes: make(map[string]*sessionEpisodeState),
	}
}

func (m *memorySessionLivenessTracker) checkFreshness(
	sessionName string,
	window time.Duration,
	sp runtime.Provider,
	now time.Time,
	startedAt time.Time,
	emitStale func(episodeID string, staleSince time.Time, lastActivity time.Time),
) bool {
	if window <= 0 {
		return false
	}
	lastActivity, err := sp.GetLastActivity(sessionName)
	if err != nil {
		// Provider error: fail-open (don't escalate on probe errors).
		return false
	}

	// A zero last-activity means the session has never been observed active.
	// Treat it as stale only once the freshness window has elapsed since the
	// controller started, to avoid false positives on fresh city startup.
	isStale := false
	if lastActivity.IsZero() {
		isStale = now.Sub(startedAt) > window
	} else {
		isStale = now.Sub(lastActivity) > window
	}

	if !isStale {
		m.recordFreshTick(sessionName)
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.episodeFor(sessionName)
	if !s.stale {
		s.stale = true
		s.alertSent = false
		s.episodeID = uuid.New().String()
		s.staleSince = now
	}
	if !s.alertSent {
		emitStale(s.episodeID, s.staleSince, lastActivity)
		s.alertSent = true
	}
	return true
}

func (m *memorySessionLivenessTracker) recordFreshTick(sessionName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.episodes[sessionName]; ok {
		s.stale = false
		s.alertSent = false
	}
}

func (m *memorySessionLivenessTracker) episodeFor(sessionName string) *sessionEpisodeState {
	if s, ok := m.episodes[sessionName]; ok {
		return s
	}
	s := &sessionEpisodeState{}
	m.episodes[sessionName] = s
	return s
}
