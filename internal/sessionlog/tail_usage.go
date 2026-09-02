package sessionlog

import (
	"encoding/json"
	"os"
)

// TailUsage is the per-invocation token usage parsed from one
// usage-bearing entry in the tail of a session transcript. It is the
// family-generic shape shared by the claude (ExtractTailUsage) and codex
// (ExtractCodexTailUsage) extractors.
type TailUsage struct {
	// EntryUUID is the family-specific transcript entry identifier: the
	// Claude DAG uuid (the LAST entry observed when one API response spans
	// several content-block entries) or the codex token_count line timestamp.
	EntryUUID string
	// MessageID is the family-specific invocation collapse identity used
	// for deduplication: the Claude provider message id (msg_*, shared by
	// every content-block entry of one API response) or the codex
	// cumulative-total identity ("total:<total_tokens>", shared by
	// duplicate token_count emissions). Empty when the transcript entry
	// carries no collapse identity.
	MessageID string
	// Model is the provider model identifier that produced the entry.
	Model string
	// InputTokens is the non-cached prompt token count.
	InputTokens int
	// OutputTokens is the completion token count.
	OutputTokens int
	// ReasoningTokens is the provider-reported reasoning token count when the
	// provider exposes it separately from completion/output tokens.
	ReasoningTokens int
	// CacheReadTokens is the cached prompt tokens read for the invocation.
	CacheReadTokens int
	// CacheCreationTokens is the tokens written into the prompt cache.
	CacheCreationTokens int
	// ContextWindowTokens is the model context window reported by the
	// provider for this invocation, when available.
	ContextWindowTokens int
}

// ExtractTailUsage reads the tail of a session transcript and returns one
// usage-bearing TailUsage per API invocation, in file order. Claude Code
// writes one assistant entry PER CONTENT BLOCK of a single response — each
// with a distinct entry uuid but the same message.id and an identical copy
// of usage — so entries sharing a message.id are collapsed to a single
// TailUsage (the last entry observed wins). Entries without a message.id
// stand alone. Entries without a uuid or with all-zero usage are skipped;
// malformed lines are tolerated silently (mirroring ExtractTailMeta). The
// scan window is the last tailChunkSize bytes, so usage that scrolled past
// the window is not returned.
func ExtractTailUsage(path string) ([]TailUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	data, _, err := readTail(f)
	if err != nil {
		return nil, err
	}
	return parseTailUsage(data), nil
}

// maxUsageScanBytes caps how far ExtractTailUsageSince will grow its scan
// window when it cannot reach the cursor entry. It bounds the worst case (a
// long-lived transcript whose cursor is stale or absent) while still covering
// transcripts orders of magnitude larger than the fixed tail window.
const maxUsageScanBytes = 16 * 1024 * 1024

// ExtractTailUsageSince returns usage-bearing invocations from the tail of a
// transcript, growing the scan window until the entry identified by cursorID
// is inside it — so no invocation is skipped merely because more than
// tailChunkSize bytes were appended since the last extraction.
//
// The fixed-window ExtractTailUsage silently drops any invocation that
// scrolled past the last 64KB between two extractions. The persisted cursor
// (session.MetadataKeyInvocationUsageCursor) is a message identity, not a byte
// offset, so a later pass cannot reach back for what it missed and the usage
// is lost permanently. Growing the window until the cursor is visible closes
// that gap without introducing new persisted state: the caller's existing
// cursor filter still decides what is new.
//
// The window doubles from tailChunkSize until the cursor entry is found, the
// whole file is in the window, or maxUsageScanBytes is reached. An empty
// cursorID (a session with no prior extraction) reads a single tailChunkSize
// window, matching ExtractTailUsage: there is no anchor to reach back to, and
// backfilling a full transcript on first sight is not this function's job.
// Callers may receive entries at or before the cursor and must still filter.
func ExtractTailUsageSince(path, cursorID string) ([]TailUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	if cursorID == "" {
		data, _, err := readTail(f)
		if err != nil {
			return nil, err
		}
		return parseTailUsage(data), nil
	}

	for window := int64(tailChunkSize); ; window *= 2 {
		data, _, truncated, err := readTailWindow(f, window)
		if err != nil {
			return nil, err
		}
		usages := parseTailUsage(data)
		// Reached the cursor, or the whole file is in view: nothing older can
		// still be owed. Either way this window is complete.
		if !truncated || containsCursor(usages, cursorID) {
			return usages, nil
		}
		if window >= maxUsageScanBytes {
			// Cap hit with the cursor still out of view: return the widest
			// window rather than nothing, so a stale cursor degrades to the
			// old bounded behavior instead of losing everything.
			return usages, nil
		}
	}
}

// containsCursor reports whether any extracted invocation carries the cursor
// identity, under the same message-id-else-entry-uuid rule the telemetry
// cursor is written with.
func containsCursor(usages []TailUsage, cursorID string) bool {
	for _, u := range usages {
		if u.MessageID == cursorID || u.EntryUUID == cursorID {
			return true
		}
	}
	return false
}

// parseTailUsage extracts invocations from an already-read transcript window.
func parseTailUsage(data []byte) []TailUsage {
	var usages []TailUsage
	// byMessageID maps a message identity to its index in usages so the
	// content-block copies of one API response collapse to a single entry.
	byMessageID := make(map[string]int)
	for _, line := range splitLines(data) {
		var entry tailEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "assistant" || entry.UUID == "" || len(entry.Message) == 0 {
			continue
		}
		var msg assistantMessage
		if err := json.Unmarshal(unwrapJSONString(entry.Message), &msg); err != nil {
			continue
		}
		if msg.Usage == nil {
			continue
		}
		u := TailUsage{
			EntryUUID:           entry.UUID,
			MessageID:           msg.ID,
			Model:               msg.Model,
			InputTokens:         msg.Usage.InputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
		}
		if u.InputTokens <= 0 && u.OutputTokens <= 0 && u.CacheReadTokens <= 0 && u.CacheCreationTokens <= 0 {
			continue
		}
		if u.MessageID != "" {
			if i, seen := byMessageID[u.MessageID]; seen {
				usages[i] = u
				continue
			}
			byMessageID[u.MessageID] = len(usages)
		}
		usages = append(usages, u)
	}
	return usages
}

// ExtractTailUsageSinceFromSearchPaths reads cursor-aware tail usage only
// after verifying path resolves under one of the configured session-log
// search roots. Mirrors ExtractTailUsageFromSearchPaths.
func ExtractTailUsageSinceFromSearchPaths(searchPaths []string, path, cursorID string) ([]TailUsage, error) {
	safePath, err := validateSearchPathFile(searchPaths, path)
	if err != nil {
		return nil, err
	}
	return ExtractTailUsageSince(safePath, cursorID)
}

// ExtractTailUsageFromSearchPaths reads tail usage only after verifying
// path resolves under one of the configured session-log search roots.
// Mirrors ExtractTailMetaFromSearchPaths.
func ExtractTailUsageFromSearchPaths(searchPaths []string, path string) ([]TailUsage, error) {
	safePath, err := validateSearchPathFile(searchPaths, path)
	if err != nil {
		return nil, err
	}
	return ExtractTailUsage(safePath)
}
