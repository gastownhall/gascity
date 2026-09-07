package worker

import (
	"bytes"
	"context"
	"os"
	"time"
)

const (
	messageReceiptTimeout      = 5 * time.Second
	messageReceiptPollInterval = 100 * time.Millisecond
)

// Capture and confirmation run inside the session's delivery lock, so another
// submit cannot supply the matching record for an undelivered identical prompt.
func (h *SessionHandle) confirmMessageFromHistory(ctx context.Context, text string) func() bool {
	id := h.currentSessionID()
	info, err := h.manager.Get(id)
	if err != nil || info.SessionKey == "" {
		return nil
	}
	path, err := h.manager.KeyedTranscriptPath(id, h.adapter.SearchPaths)
	if err != nil || path == "" {
		return nil
	}
	file, err := os.Stat(path)
	if err != nil {
		return nil
	}
	req := LoadRequest{Provider: h.historyProvider(info), TranscriptPath: path, GCSessionID: id}
	before, err := h.adapter.LoadHistory(req)
	if err != nil || !receiptHistoryUsable(before) {
		return nil
	}
	return func() bool {
		// Providers may persist the input after their terminal busy indicator is
		// gone. Wait only for publication; contradictory proof still fails at once.
		confirmationCtx, cancel := context.WithTimeout(ctx, messageReceiptTimeout)
		defer cancel()
		ticker := time.NewTicker(messageReceiptPollInterval)
		defer ticker.Stop()
		for {
			if confirmationCtx.Err() != nil {
				return false
			}
			current, err := h.manager.Get(id)
			if err != nil || current.SessionKey != info.SessionKey || h.historyProvider(current) != req.Provider {
				return false
			}
			currentPath, err := h.manager.KeyedTranscriptPath(id, h.adapter.SearchPaths)
			if err != nil || currentPath != path {
				return false
			}
			currentFile, err := os.Stat(path)
			if err != nil || !os.SameFile(file, currentFile) || currentFile.Size() < file.Size() {
				return false
			}
			after, err := h.adapter.LoadHistory(req)
			if err != nil || !receiptHistoryUsable(after) || after.TranscriptStreamID != before.TranscriptStreamID || after.ProviderSessionID != before.ProviderSessionID || len(after.Entries) < len(before.Entries) {
				return false
			}
			priorIDs := make(map[string]bool, len(before.Entries))
			for i, previous := range before.Entries {
				if previous.ID != after.Entries[i].ID || !bytes.Equal(previous.Provenance.Raw, after.Entries[i].Provenance.Raw) {
					return false
				}
				priorIDs[previous.ID] = true
			}
			users, matching := 0, 0
			for _, entry := range after.Entries[len(before.Entries):] {
				if entry.Actor == ActorUser {
					users++
					// Text is only a display projection of the first text block.
					// A receipt must contain the complete, unaugmented input.
					if entry.Kind == "user" && entry.Status == ResultStatusFinal && entry.ID != "" && !priorIDs[entry.ID] &&
						len(entry.Blocks) == 1 && entry.Blocks[0].Kind == BlockKindText && entry.Blocks[0].Text == text {
						matching++
					}
				}
			}
			if users > 0 {
				return confirmationCtx.Err() == nil && users == 1 && matching == 1
			}
			select {
			case <-confirmationCtx.Done():
				return false
			case <-ticker.C:
			}
		}
	}
}

func receiptHistoryUsable(history *HistorySnapshot) bool {
	return history != nil && history.TranscriptStreamID != "" && history.ProviderSessionID != "" &&
		len(history.Entries) > 0 && len(history.Diagnostics) == 0 && history.Pagination == nil &&
		!history.TailState.Degraded && !history.Continuity.HasBranches &&
		(history.Continuity.Status == ContinuityStatusContinuous || history.Continuity.Status == ContinuityStatusCompacted)
}
