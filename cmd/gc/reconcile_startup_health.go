package main

import (
	"context"
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// startupFailureKind classifies one failed start for its episode: a start
// that died during startup is a startup death, which leaves a
// start-stderr.log for the escalation; otherwise a deadline is a timeout and
// anything else is other.
func startupFailureKind(err error, died bool) session.FailureKind {
	switch {
	case died:
		return session.FailureKindStartupDeath
	case errors.Is(err, context.DeadlineExceeded):
		return session.FailureKindTimeout
	}
	return session.FailureKindOther
}

// fail accrues one failed start on key's episode (legacy's
// recordStartupHealthFailure, START-031): see startupFailureKind for how it is
// classified, and the episode quarantines key once it reaches
// defaultMaxWakeAttempts. name is the failed attempt's runtime session name,
// where its start-stderr.log lives. A read error accrues nothing.
func (r startupHealthRecord) fail(key, name string, err error, died bool, now time.Time) error {
	front := sessionFrontDoor(r.store)
	prior, loadErr := front.LoadStartupHealthEpisode(key)
	if loadErr != nil {
		return loadErr
	}
	prior.SessionName = key
	prior.RuntimeSessionName = name
	return front.SaveStartupHealthEpisode(session.RecordStartupFailure(prior, startupFailureKind(err, died), err.Error(), now, defaultMaxWakeAttempts, defaultQuarantineDuration))
}
