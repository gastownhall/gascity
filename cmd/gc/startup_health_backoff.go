package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// startupBackoffUntil returns the instant before which the next start attempt
// for a session with consecutiveFailures consecutive cold-start failures must
// be held back, or the zero time when it should retry immediately.
//
// The first failure gets no hold: a lone spawn flake must not delay recovery.
// From the second the interval doubles from defaultStartupBackoffBase and is
// clamped at defaultStartupBackoffCap. The shift is computed on a duration
// bounded by the cap rather than by shifting the exponent, so a pathological
// counter (the 2,306-failure episode in sr-xf2xj) cannot overflow it.
func startupBackoffUntil(consecutiveFailures int, now time.Time) time.Time {
	if consecutiveFailures < 2 {
		return time.Time{}
	}
	hold := defaultStartupBackoffBase
	for i := 2; i < consecutiveFailures; i++ {
		hold *= 2
		if hold >= defaultStartupBackoffCap {
			hold = defaultStartupBackoffCap
			break
		}
	}
	return now.Add(hold)
}

// emitStartupHealthAlert records the once-per-episode escalation for a session
// whose consecutive cold-start failures crossed the quarantine threshold, and
// returns the episode with its disposition latched to sent so no further
// failure in the same episode re-alerts.
//
// It is the consumer the raw per-attempt session.cold_start_timeout event
// never had: that event fired 2,306 times over 42 hours and nothing read it,
// so the storm was invisible until someone grepped the event stream by hand
// (sr-xf2xj). The human-readable line goes to the reconciler's stderr, where
// the rest of the session-lifecycle narration lands.
func emitStartupHealthAlert(
	rec events.Recorder,
	stderr io.Writer,
	episode sessionpkg.StartupHealthEpisode,
	template string,
) sessionpkg.StartupHealthEpisode {
	if episode.AlertDisposition != sessionpkg.AlertDispositionPending {
		return episode
	}
	episode.AlertDisposition = sessionpkg.AlertDispositionSent
	hold := episode.StartHoldUntil()
	msg := fmt.Sprintf(
		"COLD-START STORM: session=%s template=%s %d consecutive %s start failures since %s. "+
			"Starts held back until %s and retried on a capped backoff. "+
			"Inspect with: gc doctor",
		episode.SessionName, template, episode.ConsecutiveCount, startupHealthAlertKind(episode),
		formatStartupHealthAlertTime(episode.FirstFailureAt),
		formatStartupHealthAlertTime(hold),
	)
	fmt.Fprintln(stderr, "session reconciler: "+msg) //nolint:errcheck
	if rec == nil {
		return episode
	}
	// LastDetail is deliberately excluded: provider error text can carry
	// credentials, exactly as doctor_startup_health.go keeps it out of its
	// rendered check result.
	payload, _ := json.Marshal(map[string]any{
		"session_name":      episode.SessionName,
		"template":          template,
		"consecutive_count": episode.ConsecutiveCount,
		"kind":              startupHealthAlertKind(episode),
		"first_failure_at":  formatStartupHealthAlertTime(episode.FirstFailureAt),
		"held_until":        formatStartupHealthAlertTime(hold),
	})
	rec.Record(events.Event{
		Type:    events.SessionStartupHealthAlert,
		Actor:   "controller",
		Subject: episode.SessionName,
		Message: msg,
		Payload: payload,
	})
	return episode
}

func startupHealthAlertKind(episode sessionpkg.StartupHealthEpisode) string {
	if kind := string(episode.Kind); kind != "" {
		return kind
	}
	return "unspecified"
}

func formatStartupHealthAlertTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}
