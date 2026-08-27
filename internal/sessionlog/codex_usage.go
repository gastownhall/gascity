package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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
	scan := &codexTailScan{
		truncated:          truncated,
		anchorFirstTotal:   truncated,
		usageModelsByTotal: make(map[int]string),
	}
	for i := 0; i < len(lines); i++ {
		var entry codexRawEntry
		if err := json.Unmarshal(lines[i], &entry); err != nil {
			if i == len(lines)-1 && (i != 0 || !startsMidLine) {
				scan.malformedTail = true
			}
			continue
		}

		var payload codexUsagePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			continue
		}
		if entry.Type == "turn_context" && payload.Model != "" {
			scan.latestModel = payload.Model
			continue
		}
		if entry.Type == "event_msg" && payload.Type == "token_count" && payload.Info != nil {
			scan.observeTokenCount(payload.Info)
		}
	}
	return scan.result()
}

// codexTailScan folds Codex rollout tail entries into the latest model and the
// latest attributable usage. A tail-only read keeps its first positive
// cumulative total only as an unattributable duplicate anchor; a later distinct
// total pairs only with an in-window turn_context, so usage never relabels
// another model's work.
type codexTailScan struct {
	truncated           bool
	latestModel         string
	usageModel          string
	latestUsage         *codexUsageInfo
	latestUsageTotal    int
	hasLatestUsageTotal bool
	usageModelsByTotal  map[int]string
	malformedTail       bool
	anchorFirstTotal    bool
}

// observeTokenCount folds one non-nil token_count event payload into the scan.
func (s *codexTailScan) observeTokenCount(info *codexUsageInfo) {
	total := info.TotalTokenUsage.TotalTokens
	if total <= 0 {
		s.hasLatestUsageTotal = false
		s.latestUsage = info
		s.usageModel = s.latestModel
		return
	}
	if firstModel, seen := s.usageModelsByTotal[total]; seen {
		if s.hasLatestUsageTotal && total == s.latestUsageTotal {
			s.latestUsage = info
			s.usageModel = firstModel
		}
		return
	}
	s.usageModelsByTotal[total] = s.latestModel
	if s.anchorFirstTotal {
		// A tail-only read cannot tell whether its first cumulative total is new
		// or a re-emission of a snapshot before the window. Keep it only as a
		// duplicate anchor; assigning its usage to the current turn_context could
		// relabel another model's work. A later distinct total is attributable
		// again.
		s.anchorFirstTotal = false
		s.latestUsage = nil
		s.usageModel = ""
		s.hasLatestUsageTotal = false
		return
	}
	if s.truncated && s.latestModel == "" {
		// Distinct totals after the anchor are attributable only when their
		// producing turn_context is present in the retained window. Recording the
		// empty association also prevents a later duplicate from being relabeled
		// after a model appears.
		return
	}
	s.latestUsageTotal = total
	s.hasLatestUsageTotal = true
	s.latestUsage = info
	s.usageModel = s.latestModel
}

// result assembles the TailMeta from the folded scan state, pairing usage with
// the model from the same turn and deriving bounded context occupancy.
func (s *codexTailScan) result() *TailMeta {
	model := s.latestModel
	if s.latestUsage != nil {
		// Keep usage and model from the same turn. A later turn_context may
		// select a new model before its first token_count arrives; pairing that
		// model with the prior turn's usage would produce inconsistent context.
		model = s.usageModel
	}
	if model == "" && s.latestUsage == nil && !s.malformedTail {
		return nil
	}
	result := &TailMeta{Model: model, MalformedTail: s.malformedTail}
	if s.latestUsage == nil {
		return result
	}

	contextWindow := 0
	if s.latestUsage.ModelContextWindow != nil {
		contextWindow = *s.latestUsage.ModelContextWindow
	} else {
		contextWindow = ModelContextWindow(model)
	}
	if contextWindow <= 0 {
		return result
	}

	inputTokens := s.latestUsage.LastTokenUsage.InputTokens
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
//   - ReasoningTokens = last reasoning_output_tokens
//   - CacheCreationTokens = 0 (codex reports no cache-write tokens)
//
// Model comes from the latest preceding turn_context payload.model; when no
// turn_context falls inside the tail window it is seeded from the session's
// first turn_context via a bounded head scan (codexHeadModel). A codex rollout
// normally re-announces one model on every turn_context, so that first model
// prices the tail — this keeps long-lived sessions, whose turn_context has long
// scrolled past the tail window, from minting model facts with an empty (and
// therefore unpriced) model. When the head scan instead observes two distinct
// models it declines to guess and seeds empty, so a genuine mid-rollout switch
// falls back to the honest unpriced floor rather than a possibly-wrong price
// (see codexHeadModel and the mid-switch duplicate-collapse handling below). The
// seed is empty when no turn_context exists within the head-scan cap
// (token_count itself carries no model). MessageID is the cumulative-total identity
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

	// Seed the model from the session's first turn_context before scanning the
	// tail: in a long-lived session the turn_context has scrolled out of the
	// tail window, so without this seed every recent invocation records an empty
	// model. Runs before readTail because readTail leaves the read offset at the
	// tail; codexHeadModel re-seeks to the start itself.
	seedModel, err := codexHeadModel(f)
	if err != nil {
		return nil, err
	}

	data, _, err := readTail(f, tailChunkSize)
	if err != nil {
		return nil, err
	}

	var usages []TailUsage
	// byMessageID maps a cumulative-total identity to its index in usages so
	// duplicate token_count emissions collapse to a single entry.
	byMessageID := make(map[string]int)
	turnModel := seedModel
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
		contextWindowTokens := 0
		if payload.Info.ModelContextWindow != nil {
			contextWindowTokens = *payload.Info.ModelContextWindow
		}
		u := TailUsage{
			EntryUUID:           entry.Timestamp,
			MessageID:           fmt.Sprintf("total:%d", payload.Info.TotalTokenUsage.TotalTokens),
			Model:               turnModel,
			InputTokens:         input,
			OutputTokens:        last.OutputTokens,
			ReasoningTokens:     last.ReasoningOutputTokens,
			CacheReadTokens:     last.CachedInputTokens,
			ContextWindowTokens: contextWindowTokens,
		}
		if u.InputTokens <= 0 && u.OutputTokens <= 0 && u.ReasoningTokens <= 0 && u.CacheReadTokens <= 0 {
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

// codexModelSeedScanCap bounds how far into a rollout codexHeadModel scans for
// the session's model. The first turn_context sits within the first ~60 KiB of
// real rollouts (session_meta plus the opening turn's context); 1 MiB is
// generous headroom while keeping the scan bounded for multi-MB transcripts. A
// model not found within the cap falls back to empty — no worse than before the
// seed existed.
const codexModelSeedScanCap = 1 << 20 // 1 MiB

// codexHeadModel scans from the start of the rollout for the session model,
// returning "" when none is found within codexModelSeedScanCap bytes. A codex
// rollout normally re-announces the same model on every turn_context, so the
// first occurrence names the session's model; it seeds ExtractCodexTailUsage for
// the common case where the turn_context has scrolled out of the tail window. To
// avoid mispricing a rollout that DID switch models before the tail, the scan
// keeps reading within the cap and returns "" as soon as it observes a second,
// distinct turn_context.model: an ambiguous head cannot safely name one model,
// so it falls back to the honest unpriced floor rather than guessing the first.
// The scan is bounded by codexModelSeedScanCap, so a long session never re-reads
// its whole transcript; a switch that occurs after the cap but before the tail
// window is not observable here and remains seeded with the first model.
func codexHeadModel(r io.ReadSeeker) (string, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(io.LimitReader(r, codexModelSeedScanCap))
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	seed := ""
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry codexRawEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "turn_context" {
			continue
		}
		var payload codexUsagePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			continue
		}
		if payload.Model == "" {
			continue
		}
		if seed == "" {
			seed = payload.Model
			continue
		}
		if payload.Model != seed {
			// A genuine mid-rollout switch within the head window: the head cannot
			// name a single session model, so seed empty and let pricing fall back
			// to the unpriced floor instead of a possibly-wrong first model.
			return "", nil
		}
	}
	// A truncated final line at the cap boundary is expected and tolerated (same
	// posture as splitLines); an oversized pre-turn_context line likewise stops
	// the scan early and degrades to an empty seed (the pre-seed behavior).
	// scanner.Err() is intentionally ignored.
	return seed, nil
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
