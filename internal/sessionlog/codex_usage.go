package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
)

// codexTokenUsage mirrors the token-usage object the codex CLI embeds in
// event_msg token_count payloads (both total_token_usage and
// last_token_usage share this shape).
type codexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// codexUsagePayload is the subset of an event_msg payload needed for usage
// extraction. Info is null on rate-limit-only refreshes.
type codexUsagePayload struct {
	Type  string `json:"type"`
	Model string `json:"model"` // turn_context payloads only
	Info  *struct {
		TotalTokenUsage codexTokenUsage `json:"total_token_usage"`
		LastTokenUsage  codexTokenUsage `json:"last_token_usage"`
	} `json:"info"`
}

// ExtractCodexTailUsage reads the tail of a codex rollout transcript and
// returns one usage-bearing TailUsage per API call, in file order. The codex
// CLI writes an event_msg token_count line after each API call within a
// turn: last_token_usage is the per-call usage, total_token_usage is the
// strictly increasing session cumulative. Mapping (verified against real
// rollouts, where total_tokens = input_tokens + output_tokens):
//
//   - InputTokens = last input_tokens - cached_input_tokens (cached input is
//     a subset of input; clamped at zero)
//   - CacheReadTokens = last cached_input_tokens
//   - OutputTokens = last output_tokens (reasoning_output_tokens is a subset
//     of output_tokens and must not be added)
//   - CacheCreationTokens = 0 (codex reports no cache-write tokens)
//
// Model comes from the latest preceding turn_context payload.model — empty
// when no turn_context falls inside the tail window (token_count itself
// carries no model). MessageID is the cumulative-total identity
// ("total:<total_tokens>") so the exact-duplicate token_count emissions the
// CLI produces collapse to a single entry (the last observed wins, except a
// first-observed non-empty Model is kept — a duplicate re-emitted after a
// model-switching turn_context must not relabel the invocation), and
// EntryUUID is the line timestamp. token_count lines with null info
// (rate-limit-only refreshes) and all-zero per-call usage are skipped;
// malformed lines are tolerated silently. The scan window is the last
// tailChunkSize bytes, so usage that scrolled past the window is not
// returned.
func ExtractCodexTailUsage(path string) ([]TailUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	data, _, err := readTail(f, tailChunkSize)
	if err != nil {
		return nil, err
	}

	var usages []TailUsage
	// byMessageID maps a cumulative-total identity to its index in usages so
	// duplicate token_count emissions collapse to a single entry.
	byMessageID := make(map[string]int)
	var turnModel string
	for _, line := range splitLines(data) {
		var entry codexRawEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		var payload codexUsagePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			continue
		}
		if entry.Type == "turn_context" {
			if payload.Model != "" {
				turnModel = payload.Model
			}
			continue
		}
		if entry.Type != "event_msg" || payload.Type != "token_count" || payload.Info == nil {
			continue
		}
		last := payload.Info.LastTokenUsage
		input := last.InputTokens - last.CachedInputTokens
		if input < 0 {
			input = 0
		}
		u := TailUsage{
			EntryUUID:       entry.Timestamp,
			MessageID:       fmt.Sprintf("total:%d", payload.Info.TotalTokenUsage.TotalTokens),
			Model:           turnModel,
			InputTokens:     input,
			OutputTokens:    last.OutputTokens,
			CacheReadTokens: last.CachedInputTokens,
		}
		if u.InputTokens <= 0 && u.OutputTokens <= 0 && u.CacheReadTokens <= 0 {
			continue
		}
		if i, seen := byMessageID[u.MessageID]; seen {
			// The CLI re-emits the prior turn's final cumulative snapshot
			// after a new turn_context; the first-observed model is the one
			// that produced the invocation, so the collapse refreshes the
			// rest of the entry but never relabels a non-empty model.
			if usages[i].Model != "" {
				u.Model = usages[i].Model
			}
			usages[i] = u
			continue
		}
		byMessageID[u.MessageID] = len(usages)
		usages = append(usages, u)
	}
	return usages, nil
}

// ExtractCodexTailUsageFromSearchPaths reads codex tail usage only after
// verifying path resolves under one of the merged codex session roots (the
// defaults plus searchPaths). Mirrors ExtractTailUsageFromSearchPaths.
func ExtractCodexTailUsageFromSearchPaths(searchPaths []string, path string) ([]TailUsage, error) {
	safePath, err := validateSearchPathFile(mergeCodexSearchPaths(searchPaths), path)
	if err != nil {
		return nil, err
	}
	return ExtractCodexTailUsage(safePath)
}
