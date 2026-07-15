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

type codexUsageInfo struct {
	TotalTokenUsage    codexTokenUsage `json:"total_token_usage"`
	LastTokenUsage     codexTokenUsage `json:"last_token_usage"`
	ModelContextWindow *int            `json:"model_context_window"`
}

// codexUsagePayload is the subset of an event_msg payload needed for usage
// extraction. Info is null on rate-limit-only refreshes.
type codexUsagePayload struct {
	Type  string          `json:"type"`
	Model string          `json:"model"` // turn_context payloads only
	Info  *codexUsageInfo `json:"info"`
}

// ExtractCodexTailMeta reads model and context metadata from the tail of a
// Codex rollout transcript. Context usage comes from the latest distinct
// event_msg token_count whose info is not null, paired with its most recent
// preceding turn_context model. Duplicate cumulative totals retain their
// first-observed model because Codex can re-emit a prior turn's final snapshot
// after the next turn_context. When the read window is truncated, its first
// positive cumulative total is kept only as an unattributable duplicate anchor;
// a later distinct total can be paired only with an in-window turn_context.
// When no attributable usage exists, the latest turn_context still supplies
// model-only metadata. Codex input_tokens already includes cached_input_tokens,
// so context occupancy uses input_tokens directly.
func ExtractCodexTailMeta(path string) (*TailMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	data, startsMidLine, truncated, err := readTailWindow(f, tailChunkSize)
	if err != nil {
		return nil, err
	}
	return extractCodexTailMetaFromLines(splitLines(data), startsMidLine, truncated), nil
}

// ExtractCodexTailMetaFromSearchPaths reads Codex tail metadata only after
// verifying path resolves under one of the merged Codex session roots (the
// defaults plus searchPaths).
func ExtractCodexTailMetaFromSearchPaths(searchPaths []string, path string) (*TailMeta, error) {
	safePath, err := validateSearchPathFile(mergeCodexSearchPaths(searchPaths), path)
	if err != nil {
		return nil, err
	}
	return ExtractCodexTailMeta(safePath)
}

func extractCodexTailMetaFromLines(lines [][]byte, startsMidLine, truncated bool) *TailMeta {
	var (
		latestModel         string
		usageModel          string
		latestUsage         *codexUsageInfo
		latestUsageTotal    int
		hasLatestUsageTotal bool
		usageModelsByTotal  = make(map[int]string)
		malformedTail       bool
		anchorFirstTotal    = truncated
	)
	for i := 0; i < len(lines); i++ {
		var entry codexRawEntry
		if err := json.Unmarshal(lines[i], &entry); err != nil {
			if i == len(lines)-1 && (i != 0 || !startsMidLine) {
				malformedTail = true
			}
			continue
		}

		var payload codexUsagePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			continue
		}
		if entry.Type == "turn_context" && payload.Model != "" {
			latestModel = payload.Model
			continue
		}
		if entry.Type == "event_msg" && payload.Type == "token_count" && payload.Info != nil {
			total := payload.Info.TotalTokenUsage.TotalTokens
			if total > 0 {
				if firstModel, seen := usageModelsByTotal[total]; seen {
					if hasLatestUsageTotal && total == latestUsageTotal {
						latestUsage = payload.Info
						usageModel = firstModel
					}
					continue
				}
				usageModelsByTotal[total] = latestModel
				if anchorFirstTotal {
					// A tail-only read cannot tell whether its first cumulative
					// total is new or a re-emission of a snapshot before the
					// window. Keep it only as a duplicate anchor; assigning its
					// usage to the current turn_context could relabel another
					// model's work. A later distinct total is attributable again.
					anchorFirstTotal = false
					latestUsage = nil
					usageModel = ""
					hasLatestUsageTotal = false
					continue
				}
				if truncated && latestModel == "" {
					// Distinct totals after the anchor are attributable only when
					// their producing turn_context is present in the retained
					// window. Recording the empty association also prevents a
					// later duplicate from being relabeled after a model appears.
					continue
				}
				latestUsageTotal = total
				hasLatestUsageTotal = true
			} else {
				hasLatestUsageTotal = false
			}
			latestUsage = payload.Info
			usageModel = latestModel
		}
	}

	model := latestModel
	if latestUsage != nil {
		// Keep usage and model from the same turn. A later turn_context may
		// select a new model before its first token_count arrives; pairing that
		// model with the prior turn's usage would produce inconsistent context.
		model = usageModel
	}
	if model == "" && latestUsage == nil && !malformedTail {
		return nil
	}
	result := &TailMeta{Model: model, MalformedTail: malformedTail}
	if latestUsage == nil {
		return result
	}

	contextWindow := 0
	if latestUsage.ModelContextWindow != nil {
		contextWindow = *latestUsage.ModelContextWindow
	} else {
		contextWindow = ModelContextWindow(model)
	}
	if contextWindow <= 0 {
		return result
	}

	inputTokens := latestUsage.LastTokenUsage.InputTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	result.ContextUsage = &ContextUsage{
		InputTokens:   inputTokens,
		Percentage:    boundedContextPercentage(inputTokens, contextWindow),
		ContextWindow: contextWindow,
	}
	return result
}

func boundedContextPercentage(inputTokens, contextWindow int) int {
	if inputTokens <= 0 || contextWindow <= 0 {
		return 0
	}
	if inputTokens >= contextWindow {
		return 100
	}

	// Find floor(inputTokens*100/contextWindow) without multiplying the
	// untrusted token count. ceil(pct*contextWindow/100) is the smallest input
	// that earns pct; splitting the window first keeps every product in range.
	windowHundreds := contextWindow / 100
	windowRemainder := contextWindow % 100
	for percentage := 99; percentage > 0; percentage-- {
		threshold := windowHundreds * percentage
		remainderProduct := windowRemainder * percentage
		threshold += remainderProduct / 100
		if remainderProduct%100 != 0 {
			threshold++
		}
		if inputTokens >= threshold {
			return percentage
		}
	}
	return 0
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
