package session

import (
	"errors"
	"fmt"
	"strconv"
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

	// SessionWaitLookupLimit bounds per-session wait bead lookups.
	SessionWaitLookupLimit = 1000
)

// WakeConflictError reports a lifecycle state that cannot accept an explicit
// wake request.
type WakeConflictError struct {
	SessionID string
	State     string
}

func (e *WakeConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("session %s is %s", e.SessionID, e.State)
}

// WakeConflictState extracts the conflicting lifecycle state from err.
func WakeConflictState(err error) (string, bool) {
	var conflict *WakeConflictError
	if errors.As(err, &conflict) && conflict != nil {
		return conflict.State, true
	}
	return "", false
}

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

// ListSessionWaitBeads returns open durable wait beads for one session.
func ListSessionWaitBeads(store beads.Store, sessionID string) ([]beads.Bead, error) {
	if store == nil || sessionID == "" {
		return nil, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	waits, err := store.List(beads.ListQuery{
		Status: "open",
		Label:  "session:" + sessionID,
		Limit:  SessionWaitLookupLimit + 1,
		Sort:   beads.SortCreatedDesc,
	})
	if err != nil {
		return nil, err
	}
	capped := len(waits) > SessionWaitLookupLimit
	if capped {
		waits = waits[:SessionWaitLookupLimit]
	}
	result := make([]beads.Bead, 0, len(waits))
	for _, wait := range waits {
		if !IsWaitBead(wait) {
			continue
		}
		if wait.Metadata["session_id"] != sessionID {
			continue
		}
		result = append(result, wait)
	}
	if capped {
		return result, beads.LookupLimitError{Kind: "wait", Label: "session:" + sessionID, Limit: SessionWaitLookupLimit}
	}
	return result, nil
}

// WaitNudgeIDs returns queued nudge IDs for the session's currently open waits.
func WaitNudgeIDs(store beads.Store, sessionID string) ([]string, error) {
	waits, err := ListSessionWaitBeads(store, sessionID)
	if err != nil && !beads.IsLookupLimitError(err) {
		return nil, err
	}
	ids := make([]string, 0, len(waits))
	seen := make(map[string]bool, len(waits))
	for _, wait := range waits {
		nudgeID := wait.Metadata["nudge_id"]
		if nudgeID == "" || seen[nudgeID] {
			continue
		}
		seen[nudgeID] = true
		ids = append(ids, nudgeID)
	}
	return ids, err
}

// CancelWaitsAndCollectNudgeIDs marks all waits for the session terminal and
// returns every queued wait-nudge ID discovered across capped lookup pages.
func CancelWaitsAndCollectNudgeIDs(store beads.Store, sessionID string, now time.Time) ([]string, bool, error) {
	return cancelWaitsAndCollectNudgeIDs(store, sessionID, now)
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
	for {
		waits, err := ListSessionWaitBeads(store, oldSessionID)
		if err != nil && !beads.IsLookupLimitError(err) {
			return err
		}
		lookupCapped := beads.IsLookupLimitError(err)
		progressed := 0
		for _, wait := range waits {
			if IsWaitTerminalState(wait.Metadata["state"]) {
				if err := store.Close(wait.ID); err != nil {
					return fmt.Errorf("closing terminal wait %s for session %s: %w", wait.ID, oldSessionID, err)
				}
				progressed++
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
			progressed++
		}
		if !lookupCapped {
			return nil
		}
		if progressed == 0 {
			return err
		}
	}
}

// WakeSession clears hold/quarantine state and cancels open waits, returning
// any queued wait-nudge IDs that should be eagerly withdrawn.
func WakeSession(store beads.Store, sessionBead beads.Bead, now time.Time) ([]string, error) {
	if store == nil || sessionBead.ID == "" {
		return nil, nil
	}
	lcInput := LifecycleInputFromMetadata(sessionBead.Status, sessionBead.Metadata)
	lcInput.Now = now
	view := ProjectLifecycle(lcInput)
	if state, conflict := lifecycleWakeConflictState(view); conflict {
		return nil, &WakeConflictError{SessionID: sessionBead.ID, State: state}
	}
	nudgeIDs, capped, err := cancelWaitsAndCollectNudgeIDs(store, sessionBead.ID, now)
	if err != nil {
		return nil, err
	}
	state := State(strings.TrimSpace(sessionBead.Metadata["state"]))
	batch := ClearWakeBlockersPatch(state, sessionBead.Metadata["sleep_reason"])
	for k, v := range RequestExplicitWakePatch(string(WakeCauseExplicit), now) {
		batch[k] = v
	}
	if view.BaseState == BaseStateArchived && view.ContinuityEligible {
		batch["archived_at"] = ""
		batch["continuity_eligible"] = "true"
	}
	if capped {
		StampWaitLookupCapMetadata(batch, "session:"+sessionBead.ID, SessionWaitLookupLimit, now, "wake-session")
	}
	if err := store.SetMetadataBatch(sessionBead.ID, batch); err != nil {
		return nil, err
	}
	return nudgeIDs, nil
}

// StampWaitLookupCapMetadata adds the shared durable wait lookup cap
// diagnostic metadata to batch.
func StampWaitLookupCapMetadata(batch map[string]string, label string, limit int, now time.Time, source string) {
	if batch == nil {
		return
	}
	if source == "" {
		source = "wait-lookup"
	}
	batch["wait_lookup_capped_at"] = now.UTC().Format(time.RFC3339)
	batch["wait_lookup_capped_label"] = label
	batch["wait_lookup_capped_limit"] = strconv.Itoa(limit)
	batch["wait_lookup_capped_source"] = source
}

func cancelWaitsAndCollectNudgeIDs(store beads.Store, sessionID string, now time.Time) ([]string, bool, error) {
	ids := []string(nil)
	seen := map[string]bool{}
	capped := false
	canceledMetadata := map[string]string{
		"state":       waitStateCanceled,
		"canceled_at": now.UTC().Format(time.RFC3339),
	}
	for {
		waits, err := ListSessionWaitBeads(store, sessionID)
		if err != nil && !beads.IsLookupLimitError(err) {
			return ids, capped, err
		}
		lookupCapped := beads.IsLookupLimitError(err)
		capped = capped || lookupCapped
		cancelIDs := make([]string, 0, len(waits))
		terminalIDs := make([]string, 0, len(waits))
		for _, wait := range waits {
			if nudgeID := wait.Metadata["nudge_id"]; nudgeID != "" && !seen[nudgeID] {
				seen[nudgeID] = true
				ids = append(ids, nudgeID)
			}
			if IsWaitTerminalState(wait.Metadata["state"]) {
				terminalIDs = append(terminalIDs, wait.ID)
				continue
			}
			cancelIDs = append(cancelIDs, wait.ID)
		}
		if len(cancelIDs) > 0 {
			if _, err := store.CloseAll(cancelIDs, canceledMetadata); err != nil {
				return ids, capped, err
			}
		}
		if len(terminalIDs) > 0 {
			if _, err := store.CloseAll(terminalIDs, nil); err != nil {
				return ids, capped, err
			}
		}
		canceled := len(cancelIDs) + len(terminalIDs)
		if !lookupCapped {
			return ids, capped, nil
		}
		if canceled == 0 {
			return ids, capped, err
		}
	}
}

// CancelWaits marks all non-terminal waits for the session as canceled.
func CancelWaits(store beads.Store, sessionID string, now time.Time) error {
	_, _, err := CancelWaitsAndCollectNudgeIDs(store, sessionID, now)
	return err
}

func beadHasLabel(b beads.Bead, want string) bool {
	for _, label := range b.Labels {
		if label == want {
			return true
		}
	}
	return false
}
