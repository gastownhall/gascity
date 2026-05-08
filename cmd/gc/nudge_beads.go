package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	nudgeBeadType  = "chore"
	nudgeBeadLabel = "gc:nudge"

	// nudgeEnqueueRollbackCloseReason is the close_reason metadata value
	// stamped on partially-created nudge beads when enqueueQueuedNudgeWithStore's
	// withNudgeQueueState transaction returns an error after the backing
	// bead was successfully created. The rollback path closes the bead to
	// avoid leaking it; BdStore.Close forwards metadata.close_reason as
	// `bd close --reason`. Without this stamp, cities running with
	// validation.on-close=error reject the rollback close and the bead leaks
	// open with metadata.state="queued".
	// The 42-character form satisfies the >=20 char validator floor.
	nudgeEnqueueRollbackCloseReason = "nudge rollback: enqueue transaction failed"
)

type nudgeReference = nudgequeue.Reference

func openNudgeBeadStore(cityPath string) beads.Store {
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil
	}
	return store
}

func findQueuedNudgeBead(store beads.Store, nudgeID string) (beads.Bead, bool, error) {
	return findNudgeBead(store, nudgeID, false)
}

func findAnyQueuedNudgeBead(store beads.Store, nudgeID string) (beads.Bead, bool, error) {
	return findNudgeBead(store, nudgeID, true)
}

func findNudgeBead(store beads.Store, nudgeID string, includeClosed bool) (beads.Bead, bool, error) {
	if store == nil || nudgeID == "" {
		return beads.Bead{}, false, nil
	}
	opts := []beads.QueryOpt(nil)
	if includeClosed {
		opts = append(opts, beads.IncludeClosed)
	}
	items, err := store.List(beads.ListQuery{
		Label:         "nudge:" + nudgeID,
		IncludeClosed: beads.HasOpt(opts, beads.IncludeClosed),
		Sort:          beads.SortCreatedDesc,
	})
	if err != nil {
		return beads.Bead{}, false, err
	}
	var fallback beads.Bead
	hasFallback := false
	for _, item := range items {
		if item.Status != "closed" {
			return item, true, nil
		}
		if !includeClosed {
			continue
		}
		if isTerminalNudgeState(item.Metadata["state"]) {
			return item, true, nil
		}
		if !hasFallback {
			fallback = item
			hasFallback = true
		}
	}
	if includeClosed && hasFallback {
		return fallback, true, nil
	}
	return beads.Bead{}, false, nil
}

func ensureQueuedNudgeBead(store beads.Store, item queuedNudge) (string, bool, error) {
	if store == nil {
		return "", false, nil
	}
	existing, ok, err := findQueuedNudgeBead(store, item.ID)
	if err != nil {
		return "", false, err
	}
	if ok {
		return existing.ID, false, nil
	}
	meta := map[string]string{
		"nudge_id":           item.ID,
		"agent":              item.Agent,
		"session_id":         item.SessionID,
		"continuation_epoch": item.ContinuationEpoch,
		"state":              "queued",
		"source":             item.Source,
		"message":            item.Message,
		"deliver_after":      item.DeliverAfter.UTC().Format(time.RFC3339),
		"expires_at":         item.ExpiresAt.UTC().Format(time.RFC3339),
		"reference_json":     marshalNudgeReference(item.Reference),
		"last_attempt_at":    formatOptionalTime(item.LastAttemptAt),
		"last_error":         item.LastError,
		"terminal_reason":    "",
		"commit_boundary":    "",
		"terminal_at":        "",
	}
	created, err := store.Create(beads.Bead{
		Title: "nudge:" + item.ID,
		Type:  nudgeBeadType,
		Labels: []string{
			nudgeBeadLabel,
			"agent:" + item.Agent,
			"nudge:" + item.ID,
			"source:" + item.Source,
		},
		Metadata: meta,
	})
	if err != nil {
		return "", false, err
	}
	return created.ID, true, nil
}

func markQueuedNudgeTerminal(store beads.Store, item queuedNudge, state, reason, commitBoundary string, now time.Time) error {
	if store == nil {
		return nil
	}
	update := map[string]string{
		"state":           state,
		"last_attempt_at": formatOptionalTime(item.LastAttemptAt),
		"last_error":      item.LastError,
		"terminal_reason": reason,
		"commit_boundary": commitBoundary,
		"terminal_at":     now.UTC().Format(time.RFC3339),
		"close_reason":    nudgeCanonicalCloseReason(state),
	}

	tryTerminalize := func(beadID string) error {
		if beadID == "" {
			return beads.ErrNotFound
		}
		if err := store.SetMetadataBatch(beadID, update); err != nil {
			if isMissingQueuedNudgeBeadErr(err, beadID) {
				return beads.ErrNotFound
			}
			return err
		}
		if err := store.Close(beadID); err != nil {
			if isMissingQueuedNudgeBeadErr(err, beadID) {
				return beads.ErrNotFound
			}
			return err
		}
		return nil
	}

	if err := tryTerminalize(item.BeadID); err == nil {
		return nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return err
	}

	b, ok, err := findAnyQueuedNudgeBead(store, item.ID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := tryTerminalize(b.ID); err != nil && !errors.Is(err, beads.ErrNotFound) {
		return err
	}
	return nil
}

// nudgeCanonicalCloseReason maps a nudge queue terminalization state code
// to a human-readable close_reason of at least 20 characters, suitable for
// use as `bd close --reason` under validation.on-close=error.
//
// markQueuedNudgeTerminal stamps the result in metadata.close_reason
// before invoking store.Close. BdStore.Close and CloseAll forward
// metadata.close_reason as the --reason argument, which allows cities
// running with validation.on-close=error to accept the close.
// Without the canonical reason, the validator rejects close calls with
// reason <20 chars, the close fails, the entire withNudgeQueueState
// transaction rolls back, and the nudge bounces between InFlight and
// Pending forever (one bead.updated event per claim attempt) until
// expires_at cuts in.
//
// Unknown codes fall back to a descriptive phrase that remains >=20
// characters after bd's validator trims whitespace. Codes already 20+
// chars pass through unchanged.
func nudgeCanonicalCloseReason(stateCode string) string {
	switch stateCode {
	case "failed":
		return "nudge failed: queue terminalization rejected delivery"
	case "expired":
		return "nudge expired past deliver-by deadline"
	case "superseded":
		return "nudge superseded by newer queued entry"
	case "injected":
		return "nudge delivered via provider injection"
	case "accepted_for_injection":
		return "nudge accepted for hook-transport injection"
	}
	if len(stateCode) >= 20 {
		return stateCode
	}
	if stateCode == "" {
		return "nudge terminalized: unknown-state"
	}
	return "nudge terminalized: " + stateCode
}

func isMissingQueuedNudgeBeadErr(err error, beadID string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, beads.ErrNotFound) {
		return true
	}
	beadID = strings.ToLower(strings.TrimSpace(beadID))
	if beadID == "" {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no issue found matching "+strings.ToLower(strconv.Quote(beadID))) ||
		strings.Contains(msg, "error resolving "+beadID+": no issue found") ||
		strings.Contains(msg, "ambiguous id") ||
		strings.Contains(msg, "use more characters to disambiguate")
}

func marshalNudgeReference(ref *nudgeReference) string {
	if ref == nil {
		return ""
	}
	data, err := json.Marshal(ref)
	if err != nil {
		return ""
	}
	return string(data)
}

func formatOptionalTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}
