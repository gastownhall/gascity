package events

import "time"

// SessionLivenessStalePayload is the typed payload for session.liveness_stale
// events. Emitted by the controller's session liveness checker when a
// configured session's last activity exceeds its freshness_window. Fires
// exactly once per stale episode; a new episode begins only after the session
// becomes active again.
type SessionLivenessStalePayload struct {
	// Session is the runtime session name that is stale.
	Session string `json:"session"`
	// EpisodeID uniquely identifies this stale episode for correlation
	// across multiple patrol ticks and downstream alerts.
	EpisodeID string `json:"episode_id"`
	// StaleSince is when the controller first observed the session as stale
	// (first tick where last activity exceeded freshness_window).
	StaleSince time.Time `json:"stale_since"`
	// FreshnessWindow is the configured maximum allowed inactivity duration.
	FreshnessWindow string `json:"freshness_window"`
	// LastActivity is the last observed activity time for the session.
	// Zero value means the session has never been observed active (or
	// the provider does not support activity tracking).
	LastActivity time.Time `json:"last_activity,omitempty"`
	// EscalateTo is the session name that will be nudged, if configured.
	// Empty when no escalation target is configured.
	EscalateTo string `json:"escalate_to,omitempty"`
}

// IsEventPayload marks SessionLivenessStalePayload as an events.Payload variant.
func (SessionLivenessStalePayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionLivenessStale, SessionLivenessStalePayload{})
}
