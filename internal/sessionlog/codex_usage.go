package sessionlog

import (
	"bytes"
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
	Type   string          `json:"type"`
	Model  string          `json:"model"` // turn_context payloads only
	Info   *codexUsageInfo `json:"info"`
	TurnID string          `json:"turn_id"`
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
// so context occupancy uses input_tokens directly. Activity follows identified
// task lifecycle events, not assistant text or usage. If the tail omits the
// lifecycle needed to establish activity, a bounded backwards scan stops at the
// latest identified start/context without expanding the usage attribution window.
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
	scan := scanCodexTailMetaFromLines(splitLines(data), startsMidLine, truncated)
	if truncated && scan.activity.state == "" && !scan.malformedTail {
		activity, err := readCodexActivity(f)
		if err != nil {
			return nil, err
		}
		scan.activity = activity
	}
	return scan.result(), nil
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
	return scanCodexTailMetaFromLines(lines, startsMidLine, truncated).result()
}

func scanCodexTailMetaFromLines(lines [][]byte, startsMidLine, truncated bool) *codexTailScan {
	scan := &codexTailScan{
		truncated:          truncated,
		anchorFirstTotal:   truncated,
		usageModelsByTotal: make(map[int]string),
	}
	for i := 0; i < len(lines); i++ {
		var entry codexRawEntry
		if err := json.Unmarshal(lines[i], &entry); err != nil {
			scan.activity = codexActivity{}
			if i == len(lines)-1 && (i != 0 || !startsMidLine) {
				scan.malformedTail = true
			}
			continue
		}

		var payload codexUsagePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			scan.activity = codexActivity{}
			if i == len(lines)-1 {
				scan.malformedTail = true
			}
			continue
		}
		scan.activity.observe(entry.Type, payload)
		if entry.Type == "turn_context" && payload.Model != "" {
			scan.latestModel = payload.Model
			continue
		}
		if entry.Type == "event_msg" && payload.Type == "token_count" && payload.Info != nil {
			scan.observeTokenCount(payload.Info)
		}
	}
	return scan
}

// codexActivity requires an in-order start/context for the same turn before
// accepting its terminal event. A late prior-turn completion cannot end the
// current task. Missing identities or malformed records discard that evidence.
type codexActivity struct {
	turnID string
	state  string
}

func (a *codexActivity) observe(entryType string, payload codexUsagePayload) {
	if entryType == "turn_context" || (entryType == "event_msg" && payload.Type == "task_started") {
		*a = codexActivity{}
		if payload.TurnID != "" {
			a.turnID, a.state = payload.TurnID, "in-turn"
		}
		return
	}
	if entryType != "event_msg" || (payload.Type != "task_complete" && payload.Type != "turn_aborted") {
		return
	}
	switch payload.TurnID {
	case "":
		*a = codexActivity{}
	case a.turnID:
		a.state = "idle"
	}
}

func readCodexActivity(r io.ReadSeeker) (codexActivity, error) {
	offset, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return codexActivity{}, err
	}
	terminals := make(map[string]bool)
	// Only the current JSONL record is retained across 64 KiB chunks. Keep
	// fragments instead of repeatedly copying a large tool-output line.
	var fragments [][]byte
	lineBytes := 0
	appendFragment := func(fragment []byte) error {
		lineBytes += len(fragment)
		// Match ReadCodexFile's maximum rollout record size.
		if lineBytes > 50*1024*1024 {
			return fmt.Errorf("scanning codex activity: rollout record exceeds 50 MiB")
		}
		if len(fragment) > 0 {
			fragments = append(fragments, fragment)
		}
		return nil
	}
	observeLine := func() (codexActivity, bool) {
		if lineBytes == 0 {
			return codexActivity{}, false
		}
		line := make([]byte, lineBytes)
		position := 0
		for i := len(fragments) - 1; i >= 0; i-- {
			position += copy(line[position:], fragments[i])
		}
		fragments, lineBytes = nil, 0
		var entry codexRawEntry
		var payload codexUsagePayload
		if json.Unmarshal(line, &entry) != nil || json.Unmarshal(entry.Payload, &payload) != nil {
			// An unreadable record could hide a newer start. Do not reach past
			// that barrier and report an older task's completion as current.
			return codexActivity{}, true
		}
		if entry.Type == "turn_context" || (entry.Type == "event_msg" && payload.Type == "task_started") {
			var activity codexActivity
			activity.observe(entry.Type, payload)
			if activity.turnID != "" && terminals[activity.turnID] {
				activity.state = "idle"
			}
			return activity, true
		}
		if entry.Type == "event_msg" && (payload.Type == "task_complete" || payload.Type == "turn_aborted") {
			if payload.TurnID == "" {
				return codexActivity{}, true
			}
			terminals[payload.TurnID] = true
		}
		return codexActivity{}, false
	}
	for offset > 0 {
		size := min(offset, int64(tailChunkSize))
		offset -= size
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return codexActivity{}, err
		}
		chunk := make([]byte, int(size))
		if _, err := io.ReadFull(r, chunk); err != nil {
			return codexActivity{}, fmt.Errorf("reading codex activity: %w", err)
		}
		for {
			newline := bytes.LastIndexByte(chunk, '\n')
			if newline < 0 {
				break
			}
			if err := appendFragment(chunk[newline+1:]); err != nil {
				return codexActivity{}, err
			}
			if activity, done := observeLine(); done {
				return activity, nil
			}
			chunk = chunk[:newline]
		}
		if err := appendFragment(chunk); err != nil {
			return codexActivity{}, err
		}
	}
	activity, _ := observeLine()
	return activity, nil
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
	activity            codexActivity
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
	if model == "" && s.latestUsage == nil && !s.malformedTail && s.activity.state == "" {
		return nil
	}
	result := &TailMeta{Model: model, MalformedTail: s.malformedTail, Activity: s.activity.state}
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
		u, ok := codexInvocationUsage(entry, payload, turnModel)
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

// codexHistoryUsage uses the records already read by the full transcript parser.
// First observations fix the invocation timestamp and model: later duplicate
// cumulative snapshots must not move usage onto a newer assistant message.
func codexHistoryUsage(entries []codexEntry) []TailUsage {
	var usages []TailUsage
	seen := make(map[string]bool)
	var model string
	for _, entry := range entries {
		if entry.raw.Type != "turn_context" && entry.raw.Type != "event_msg" {
			continue
		}
		var payload codexUsagePayload
		if json.Unmarshal(entry.raw.Payload, &payload) != nil {
			continue
		}
		if entry.raw.Type == "turn_context" && payload.Model != "" {
			model = payload.Model
		}
		if entry.raw.Type != "event_msg" || payload.Type != "token_count" || payload.Info == nil {
			continue
		}
		usage, ok := codexInvocationUsage(entry.raw, payload, model)
		if !ok || seen[usage.MessageID] {
			continue
		}
		seen[usage.MessageID] = true
		usages = append(usages, usage)
	}
	return usages
}

func codexInvocationUsage(entry codexRawEntry, payload codexUsagePayload, model string) (TailUsage, bool) {
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
