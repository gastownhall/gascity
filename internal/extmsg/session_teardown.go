package extmsg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// SessionTeardownStats summarizes one ResumePendingSessionTeardowns sweep.
type SessionTeardownStats struct {
	// Pending counts the open teardown records examined.
	Pending int
	// Finished counts records closed because their session's teardown
	// completed.
	Finished int
	// Dropped counts records closed without a teardown: their session is live
	// again, or the record names no session.
	Dropped int
}

// RecordPendingSessionTeardown durably records that the external-message
// teardown of the retired session sessionID (CloseSessionBindings) did not
// finish, so ResumePendingSessionTeardowns finishes it later. Recording is
// idempotent: an open record for sessionID is kept as it is. It takes no
// context because its caller records after the context that bounded the
// teardown has ended.
func RecordPendingSessionTeardown(store beads.MailStore, sessionID string, now time.Time) error {
	if store == (beads.MailStore{}) {
		return nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	label := sessionTeardownSessionLabel(sessionID)
	existing, err := store.List(beads.ListQuery{Label: label})
	if err != nil {
		return fmt.Errorf("list pending teardowns for session %s: %w", sessionID, err)
	}
	for _, item := range existing {
		if hasLabel(item, labelSessionTeardownBase) && item.Status != "closed" {
			return nil
		}
	}
	if _, err := store.Create(beads.Bead{
		Title:  "external-message teardown pending for retired session " + sessionID,
		Type:   "task",
		Labels: []string{labelSessionTeardownBase, label},
		Metadata: map[string]string{
			"session_id":  sessionID,
			"recorded_at": formatTime(zeroNow(now)),
		},
	}); err != nil {
		return fmt.Errorf("record pending teardown for session %s: %w", sessionID, err)
	}
	return nil
}

// ResumePendingSessionTeardowns finishes the teardowns
// RecordPendingSessionTeardown recorded. For each open record it reads the
// record's session from sessions: a session that is closed, archived or gone
// gets CloseSessionBindings run again, and its record closes once that
// succeeds; a session that is live again (a removed named session restored to
// config) keeps its external-message state, and its record closes without a
// teardown. A record whose teardown or session read fails stays open for the
// next sweep, and the sweep goes on to the other records and reports every
// failure together.
//
// ctx stops the sweep between records and, inside a teardown, between its
// items, so a sweep bounded by ctx leaves what it did not reach to the next
// one. The sweep is idempotent and safe to run on every reconciler tick.
func ResumePendingSessionTeardowns(ctx context.Context, store beads.MailStore, sessions beads.SessionStore, now time.Time) (SessionTeardownStats, error) {
	var stats SessionTeardownStats
	if err := checkContext(ctx); err != nil {
		return stats, err
	}
	if store == (beads.MailStore{}) {
		return stats, nil
	}
	if sessions == (beads.SessionStore{}) {
		return stats, errors.New("resuming pending session teardowns requires a sessions store")
	}
	items, err := store.List(beads.ListQuery{Label: labelSessionTeardownBase})
	if err != nil {
		return stats, fmt.Errorf("list pending session teardowns: %w", err)
	}
	directory := session.NewStore(sessions)
	var errs error
	for _, item := range items {
		if !hasLabel(item, labelSessionTeardownBase) || item.Status == "closed" {
			continue
		}
		if err := checkContext(ctx); err != nil {
			return stats, errors.Join(errs, err)
		}
		stats.Pending++
		tornDown, err := resumeSessionTeardown(ctx, store, sessions, directory, item, now)
		switch {
		case err != nil:
			errs = errors.Join(errs, err)
			if ctx.Err() != nil {
				return stats, errs
			}
		case tornDown:
			stats.Finished++
		default:
			stats.Dropped++
		}
	}
	return stats, errs
}

// resumeSessionTeardown finishes one pending-teardown record: it tears down
// the record's session when that session is retired, then closes the record.
// It reports whether it tore the session down; false with a nil error means
// the record was dropped (its session is live again, or it names none).
func resumeSessionTeardown(ctx context.Context, store beads.MailStore, sessions beads.SessionStore, directory session.AddressDirectory, record beads.Bead, now time.Time) (tornDown bool, err error) {
	sessionID := strings.TrimSpace(record.Metadata["session_id"])
	if sessionID != "" {
		tornDown, err = sessionRetired(directory, sessionID)
		if err != nil {
			return false, fmt.Errorf("read session %s for its pending teardown: %w", sessionID, err)
		}
	}
	if tornDown {
		if err := CloseSessionBindings(ctx, store, sessions, sessionID, now); err != nil {
			return false, fmt.Errorf("resume external-message teardown of session %s: %w", sessionID, err)
		}
	}
	if err := store.Close(record.ID); err != nil {
		return false, fmt.Errorf("close pending teardown %s of session %s: %w", record.ID, sessionID, err)
	}
	return tornDown, nil
}

// sessionRetired reports whether sessionID names a retired session: one that
// is closed, archived, or no longer in the directory.
func sessionRetired(directory session.AddressDirectory, sessionID string) (bool, error) {
	info, err := directory.ResolveAddress(sessionID, true)
	if errors.Is(err, session.ErrSessionNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return info.Closed || info.State == session.StateArchived, nil
}
