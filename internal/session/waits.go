package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// WaitBeadType identifies durable wait beads associated with sessions.
	WaitBeadType = "gate"
	// LegacyWaitBeadType is the historical durable wait bead type kept for
	// backward-compatible reads against older stores.
	LegacyWaitBeadType = "wait"
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

// IsWaitBeadType reports whether the bead type is recognized as a durable
// session wait.
func IsWaitBeadType(typ string) bool {
	switch typ {
	case WaitBeadType, LegacyWaitBeadType:
		return true
	default:
		return false
	}
}

// IsWaitBead reports whether a bead is a durable session wait. New waits are
// stored as gate beads, while legacy stores may still contain type "wait".
func IsWaitBead(b beads.Bead) bool {
	if !IsWaitBeadType(b.Type) {
		return false
	}
	if beadHasLabel(b, WaitBeadLabel) {
		return true
	}
	sessionID := b.Metadata["session_id"]
	return sessionID != "" && beadHasLabel(b, "session:"+sessionID)
}

// WaitNudgeIDs returns queued nudge IDs for the session's currently open waits.
func WaitNudgeIDs(store beads.Store, sessionID string) ([]string, error) {
	if store == nil || sessionID == "" {
		return nil, nil
	}
	waits, err := store.List(beads.ListQuery{
		Label: "session:" + sessionID,
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
		if !IsWaitBead(wait) {
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

// ReassignWaits moves open non-terminal waits from one session bead ID to
// another during canonical session repair.
func ReassignWaits(store beads.Store, oldSessionID, newSessionID string) error {
	if store == nil {
		return nil
	}
	oldSessionID = strings.TrimSpace(oldSessionID)
	newSessionID = strings.TrimSpace(newSessionID)
	if oldSessionID == "" || newSessionID == "" || oldSessionID == newSessionID {
		return nil
	}
	oldLabel := "session:" + oldSessionID
	newLabel := "session:" + newSessionID
	waits, err := store.List(beads.ListQuery{Label: oldLabel})
	if err != nil {
		return err
	}
	for _, wait := range waits {
		if wait.Status == "closed" {
			continue
		}
		if !IsWaitBead(wait) {
			continue
		}
		if wait.Metadata["session_id"] != oldSessionID {
			continue
		}
		if IsWaitTerminalState(wait.Metadata["state"]) {
			continue
		}
		labels := []string(nil)
		if !beadHasLabel(wait, newLabel) {
			labels = []string{newLabel}
		}
		if err := store.Update(wait.ID, beads.UpdateOpts{
			Labels:       labels,
			RemoveLabels: []string{oldLabel},
			Metadata:     map[string]string{"session_id": newSessionID},
		}); err != nil {
			return fmt.Errorf("reassign wait %s from session %s to %s: %w", wait.ID, oldSessionID, newSessionID, err)
		}
	}
	return nil
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
		"churn_count":       "0",
	}
	switch strings.TrimSpace(sessionBead.Metadata["state"]) {
	case "suspended", "drained":
		batch["state"] = "asleep"
	}
	sr := sessionBead.Metadata["sleep_reason"]
	if sr == "user-hold" || sr == "wait-hold" || sr == "quarantine" || sr == "context-churn" || sr == "drained" {
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
	})
	if err != nil {
		return err
	}
	canceledAt := now.UTC().Format(time.RFC3339)
	for _, wait := range waits {
		if wait.Status == "closed" {
			continue
		}
		if !IsWaitBead(wait) {
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

func beadHasLabel(b beads.Bead, want string) bool {
	for _, label := range b.Labels {
		if label == want {
			return true
		}
	}
	return false
}
