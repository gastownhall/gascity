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

// fail accrues one failed start on key's episode (legacy's
// recordStartupHealthFailure, START-031): a deadline accrues a timeout, any
// other error a failure, and the episode quarantines key once it reaches
// defaultMaxWakeAttempts. A read error accrues nothing.
func (r startupHealthRecord) fail(key string, err error, now time.Time) error {
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
	return front.SaveStartupHealthEpisode(session.RecordStartupFailure(prior, kind, err.Error(), now, defaultMaxWakeAttempts, defaultQuarantineDuration))
}

// startupHealthEpisodeIndex answers the legacy reconciler loop's per-candidate
// startup-health episode lookups from one store read per reconcile call. It is
// built lazily, at the first candidate that reaches the lookup, so a tick with
// no down wake candidate reads nothing. The caller keeps it in a variable local
// to the call: an index that outlived the tick would serve a stale episode to
// the next one.
//
// A failed read is memoized with the same error for every later candidate, so
// each takes the loop's fail-open arm and the store sees one attempt per tick.
// A partial-result batch read (some episode bead failed to parse) is not
// memoized as a failure: the index falls back to per-name loads for the rest
// of the tick, so one malformed episode bead fails open only its own session,
// as the per-name read always has.
type startupHealthEpisodeIndex struct {
	front    *session.Store
	loaded   bool
	perName  bool
	episodes map[string]session.StartupHealthEpisode
	err      error
}

// newStartupHealthEpisodeIndex returns an index that reads through front on its
// first lookup.
func newStartupHealthEpisodeIndex(front *session.Store) *startupHealthEpisodeIndex {
	return &startupHealthEpisodeIndex{front: front}
}

// lookup returns key's open episode, or the zero episode when it has none,
// matching Store.LoadStartupHealthEpisode. A failed read returns its error for
// this and every later lookup; after a partial-result batch read every lookup
// is a per-name Store.LoadStartupHealthEpisode.
func (x *startupHealthEpisodeIndex) lookup(key string) (session.StartupHealthEpisode, error) {
	if !x.loaded {
		x.episodes, x.err = x.front.OpenStartupHealthEpisodes()
		x.loaded = true
		if x.err != nil && beads.IsPartialResult(x.err) {
			x.perName = true
			x.err = nil
		}
	}
	if x.perName {
		return x.front.LoadStartupHealthEpisode(key)
	}
	if x.err != nil {
		return session.StartupHealthEpisode{}, x.err
	}
	return x.episodes[key], nil
}
