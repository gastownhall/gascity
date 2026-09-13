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
	lines, err := splitLines(data)
	if err != nil {
		return nil, err
	}
	return extractCodexTailMetaFromLines(lines, startsMidLine, truncated), nil
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
// turn_context falls inside the tail window it is recovered from the nearest
// turn_context preceding that window by a bounded backward scan
// (codexPrecedingModel). That nearest preceding announcement is the model in
// effect for the tail, so it prices the tail — this keeps long-lived sessions,
// whose turn_context has long scrolled past the tail window, from minting model
// facts with an empty (and therefore unpriced) model, and it honors a
// mid-rollout model switch wherever it happened. The recovered model is empty
// when no turn_context is in reach of that scan or the scan cannot complete —
// an unpriced model fact is a truer report than a stale one, and token_count
// itself carries no model. MessageID is the cumulative-total identity
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

	// One size snapshot feeds both windows. The seed and the tail read are
	// adjacent halves of the same file, so deriving them from separate SeekEnd
	// calls would leave the bytes appended in between — these rollouts are
	// appended to while they are read — inside neither.
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	// Recover the model in effect for the tail before scanning it: in a
	// long-lived session the turn_context has scrolled out of the tail window,
	// so without this seed every recent invocation records an empty model.
	seedModel, err := codexPrecedingModel(f, size-tailChunkSize)
	if err != nil {
		return nil, err
	}

	data, _, err := readTailAt(f, size, tailChunkSize)
	if err != nil {
		return nil, err
	}

	lines, err := splitLines(data)
	if err != nil {
		return nil, err
	}

	var usages []TailUsage
	// byMessageID maps a cumulative-total identity to its index in usages so
	// duplicate token_count emissions collapse to a single entry.
	byMessageID := make(map[string]int)
	turnModel := seedModel
	for _, line := range lines {
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
		u, ok := codexTokenCountUsage(entry, payload, turnModel)
		if !ok {
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

// codexTokenCountUsage converts a token_count entry into a TailUsage labeled
// with model, the model in effect when the invocation ran. It reports false for
// an entry that carries no usable per-call usage: a different entry type, a
// rate-limit-only refresh (null info), or an all-zero per-call usage.
func codexTokenCountUsage(entry codexRawEntry, payload codexUsagePayload, model string) (TailUsage, bool) {
	if entry.Type != "event_msg" || payload.Type != "token_count" || payload.Info == nil {
		return TailUsage{}, false
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
		Model:               model,
		InputTokens:         input,
		OutputTokens:        last.OutputTokens,
		ReasoningTokens:     last.ReasoningOutputTokens,
		CacheReadTokens:     last.CachedInputTokens,
		ContextWindowTokens: contextWindowTokens,
	}
	if u.InputTokens <= 0 && u.OutputTokens <= 0 && u.ReasoningTokens <= 0 && u.CacheReadTokens <= 0 {
		return TailUsage{}, false
	}
	return u, true
}

// codexModelSeedScanBudget bounds how far back from the tail window
// codexPrecedingModel scans for the model in effect there. Real rollouts
// re-announce the model on every turn_context, so the nearest preceding one
// normally sits within a few KiB of the window; 1 MiB is generous headroom
// while keeping the backward scan bounded for multi-MB transcripts. A model not
// found within the budget falls back to empty — no worse than before the seed
// existed.
const codexModelSeedScanBudget = 1 << 20 // 1 MiB

// codexSeedScanLineMax bounds the longest JSONL line codexPrecedingModel can
// read, matching the line budget splitLines uses for the tail window.
const codexSeedScanLineMax = 256 * 1024

// codexPrecedingModel returns the model named by the last turn_context starting
// before windowStart, the offset the tail window begins at. That is the model in
// effect for the tail when no turn_context falls inside the window itself. The
// caller passes windowStart rather than letting this re-derive it, so the seed
// and the tail read are two views of one size snapshot: on a rollout being
// appended to, two independent snapshots leave the bytes written in between in
// neither window.
//
// It returns "" when the tail window already covers the whole file (the
// in-window scan then sees every turn_context and needs no seed), when no
// turn_context appears within codexModelSeedScanBudget bytes before the window,
// and when an over-long line truncates the scan mid-range.
//
// A seek or read failure is not degraded that way: it is returned as an error
// and so fails the whole extraction. That is stricter than readTailWindowAt,
// whose preceding-byte peek swallows the same two errors and defaults to false.
// Which contract both should keep is tracked as ga-8gku9.
//
// Taking the NEAREST preceding announcement rather than the session's first
// keeps a mid-rollout model switch honored: the tail is not labeled with a model
// the session had already switched away from by the time the window starts.
func codexPrecedingModel(r io.ReadSeeker, windowStart int64) (string, error) {
	if windowStart <= 0 {
		return "", nil
	}
	rangeStart := windowStart - codexModelSeedScanBudget
	if rangeStart < 0 {
		rangeStart = 0
	}
	// The budget cut can land either inside a line or exactly on a line start,
	// so peek the preceding byte the way readTailWindow does instead of assuming
	// a partial line whenever the range is truncated. Only the decision is
	// shared: readTailWindow swallows a failed peek, this returns it (ga-8gku9).
	skipPartialFirst, err := codexStartsMidLine(r, rangeStart)
	if err != nil {
		return "", err
	}
	if _, err := r.Seek(rangeStart, io.SeekStart); err != nil {
		return "", err
	}
	// Read one max-line allowance past windowStart so the line straddling the
	// boundary is consumed whole here. readTail starts at exactly windowStart
	// with no line-boundary snapping, so the in-window scan sees only that
	// line's suffix and cannot parse it. Lines that start at or after
	// windowStart belong to the in-window scan and end this one.
	scanner := bufio.NewScanner(io.LimitReader(r, windowStart-rangeStart+codexSeedScanLineMax))
	scanner.Buffer(make([]byte, codexSeedScanLineMax), codexSeedScanLineMax)
	consumed := int64(0)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		consumed += int64(advance)
		return advance, token, err
	})

	model := ""
	lineStart := int64(0)
	for scanner.Scan() {
		start := rangeStart + lineStart
		lineStart = consumed
		if start >= windowStart {
			break
		}
		if skipPartialFirst {
			skipPartialFirst = false
			continue
		}
		if m := codexTurnContextModel(scanner.Bytes()); m != "" {
			model = m
		}
	}
	if scanner.Err() != nil {
		// A line past codexSeedScanLineMax aborts the scan mid-range, so a
		// switch announced after it is unseen. Reporting the model found before
		// it would replace an honest unpriced $0 with a plausible wrong price
		// that no later sweep corrects, so degrade to the empty seed instead.
		return "", nil
	}
	return model, nil
}

// codexStartsMidLine reports whether offset falls inside a line rather than on a
// line start, by peeking the byte before it.
func codexStartsMidLine(r io.ReadSeeker, offset int64) (bool, error) {
	if offset <= 0 {
		return false, nil
	}
	if _, err := r.Seek(offset-1, io.SeekStart); err != nil {
		return false, err
	}
	var prev [1]byte
	if _, err := io.ReadFull(r, prev[:]); err != nil {
		return false, err
	}
	return prev[0] != '\n', nil
}

// codexTurnContextModel returns the model a turn_context line announces, or ""
// when the line is not a turn_context or names no model. Malformed lines are
// tolerated silently: a scan bounded by byte offsets can begin or end inside a
// line, matching splitLines.
func codexTurnContextModel(line []byte) string {
	if len(line) == 0 {
		return ""
	}
	var entry codexRawEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return ""
	}
	if entry.Type != "turn_context" {
		return ""
	}
	var payload codexUsagePayload
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		return ""
	}
	return payload.Model
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
