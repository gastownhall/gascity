package main

import (
	"encoding/json"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	nudgeBeadType  = "chore"
	nudgeBeadLabel = "gc:nudge"
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
	items, err := store.ListByLabel("nudge:"+nudgeID, 0, opts...)
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
	beadID := item.BeadID
	if beadID == "" {
		b, ok, err := findQueuedNudgeBead(store, item.ID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		beadID = b.ID
	}
	if err := store.SetMetadataBatch(beadID, map[string]string{
		"state":           state,
		"last_attempt_at": formatOptionalTime(item.LastAttemptAt),
		"last_error":      item.LastError,
		"terminal_reason": reason,
		"commit_boundary": commitBoundary,
		"terminal_at":     now.UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	return store.Close(beadID)
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
