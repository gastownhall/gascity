package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// startupHealthRecord is the start effect's one #46 writer, routed outside
// the linted effect files, which ban SaveStartupHealthEpisode: an
// unconditional upsert of an episode record, never a session row, which v5
// §13 admits because a lost write only delays a start.
type startupHealthRecord struct{ store beads.Store }

// accrued reports whether key's episode needs a clear; a read error says no.
func (r startupHealthRecord) accrued(key string) bool {
	prior, err := sessionFrontDoor(r.store).LoadStartupHealthEpisode(key)
	return err == nil && (prior.ConsecutiveCount != 0 || !prior.QuarantinedUntil.IsZero())
}

// clear clears key's episode, as legacy's start commit does.
func (r startupHealthRecord) clear(key string) error {
	return sessionFrontDoor(r.store).SaveStartupHealthEpisode(session.ClearStartupHealthEpisode(key))
}

// fail accrues one failed start on key's episode (legacy's
// recordStartupHealthFailure, START-031): a deadline accrues a timeout, any
// other error a failure, and the episode quarantines key once it reaches
// defaultMaxWakeAttempts. Like legacy, it stamps the per-failure retry
// backoff and emits the once-per-episode startup-health alert to rec and
// stderr on crossing the threshold (sr-xf2xj). A read error accrues nothing.
func (r startupHealthRecord) fail(key, template string, err error, now time.Time, rec events.Recorder, stderr io.Writer) error {
	front := sessionFrontDoor(r.store)
	prior, loadErr := front.LoadStartupHealthEpisode(key)
	if loadErr != nil {
		return loadErr
	}
	prior.SessionName = key
	kind := session.FailureKindOther
	if errors.Is(err, context.DeadlineExceeded) {
		kind = session.FailureKindTimeout
	}
	episode := session.RecordStartupFailure(prior, kind, err.Error(), now, defaultMaxWakeAttempts, defaultQuarantineDuration)
	episode.BackoffUntil = startupBackoffUntil(episode.ConsecutiveCount, now)
	if stderr == nil {
		stderr = io.Discard
	}
	// Recorded before the save, as legacy does: a lost save re-alerts on the
	// next failure rather than losing the escalation.
	episode = emitStartupHealthAlert(rec, stderr, episode, template)
	return front.SaveStartupHealthEpisode(episode)
}
