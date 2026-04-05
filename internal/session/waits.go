package session

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// WaitBeadType identifies durable wait beads associated with sessions.
	WaitBeadType = "wait"
	// WaitBeadLabel is the common label used to locate session wait beads.
	WaitBeadLabel = "gc:wait"

	waitStateClosed   = "closed"
	waitStateCanceled = "canceled"
	waitStateExpired  = "expired"
	waitStateFailed   = "failed"
)

// IsWaitTerminalState reports whether a durable wait has reached a terminal lifecycle state.
func IsWaitTerminalState(state string) bool {
	switch state {
	case waitStateClosed, waitStateCanceled, waitStateExpired, waitStateFailed:
		return true
	default:
		return false
	}
}

// WaitNudgeIDs returns queued nudge IDs for the session's currently open waits.
func WaitNudgeIDs(store beads.Store, sessionID string) ([]string, error) {
	if store == nil || sessionID == "" {
		return nil, nil
	}
	waits, err := store.List(beads.ListQuery{
		Label: "session:" + sessionID,
		Type:  WaitBeadType,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(waits))
	seen := make(map[string]bool, len(waits))
	for _, wait := range waits {
		if wait.Status == "closed" {
			continue
		}
		if wait.Type != WaitBeadType {
			continue
		}
		if wait.Metadata["session_id"] != sessionID {
			continue
		}
		nudgeID := wait.Metadata["nudge_id"]
		if nudgeID == "" || seen[nudgeID] {
			continue
		}
		seen[nudgeID] = true
		ids = append(ids, nudgeID)
	}
	return ids, nil
}

// WakeSession clears hold/quarantine state and cancels open waits, returning
// any queued wait-nudge IDs that should be eagerly withdrawn.
func WakeSession(store beads.Store, sessionBead beads.Bead, now time.Time) ([]string, error) {
	if store == nil || sessionBead.ID == "" {
		return nil, nil
	}
	nudgeIDs, err := WaitNudgeIDs(store, sessionBead.ID)
	if err != nil {
		return nil, err
	}
	if err := CancelWaits(store, sessionBead.ID, now); err != nil {
		return nil, err
	}
	batch := map[string]string{
		"held_until":        "",
		"quarantined_until": "",
		"wait_hold":         "",
		"sleep_intent":      "",
		"wake_attempts":     "0",
	}
	sr := sessionBead.Metadata["sleep_reason"]
	if sr == "user-hold" || sr == "wait-hold" || sr == "quarantine" {
		batch["sleep_reason"] = ""
	}
	if err := store.SetMetadataBatch(sessionBead.ID, batch); err != nil {
		return nil, err
	}
	return nudgeIDs, nil
}

// CancelWaits marks all non-terminal waits for the session as canceled.
func CancelWaits(store beads.Store, sessionID string, now time.Time) error {
	if store == nil || sessionID == "" {
		return nil
	}
	waits, err := store.List(beads.ListQuery{
		Label: "session:" + sessionID,
		Type:  WaitBeadType,
	})
	if err != nil {
		return err
	}
	canceledAt := now.UTC().Format(time.RFC3339)
	for _, wait := range waits {
		if wait.Status == "closed" {
			continue
		}
		if wait.Type != WaitBeadType {
			continue
		}
		if wait.Metadata["session_id"] != sessionID {
			continue
		}
		if IsWaitTerminalState(wait.Metadata["state"]) {
			continue
		}
		if err := store.SetMetadataBatch(wait.ID, map[string]string{
			"state":       waitStateCanceled,
			"canceled_at": canceledAt,
		}); err != nil {
			return err
		}
		if err := store.Close(wait.ID); err != nil {
			return err
		}
	}
	return nil
}
