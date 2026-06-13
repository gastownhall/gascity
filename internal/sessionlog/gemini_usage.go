package sessionlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// geminiUsageTokens mirrors the tokens object gemini-cli's
// ChatRecordingService.recordMessageTokens writes onto gemini messages,
// sourced from GenerateContentResponse usageMetadata: input is
// promptTokenCount, output is candidatesTokenCount, cached is
// cachedContentTokenCount, thoughts is thoughtsTokenCount, tool is
// toolUsePromptTokenCount.
type geminiUsageTokens struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
	Tool     int `json:"tool"`
	Total    int `json:"total"`
}

// geminiUsageMessage is the subset of a recorded gemini chat message needed
// for usage extraction.
type geminiUsageMessage struct {
	ID     string             `json:"id"`
	Type   string             `json:"type"`
	Model  string             `json:"model"`
	Tokens *geminiUsageTokens `json:"tokens"`
}

// ExtractGeminiUsage reads a gemini chat recording and returns one
// usage-bearing TailUsage per API invocation, in file order. Two on-disk
// formats exist:
//
//   - legacy single-JSON object ({sessionId, projectHash, messages[]},
//     ".json" suffix) — read and unmarshalled WHOLE: the envelope cannot be
//     tail-scanned, so unlike every other extractor on this path the read is
//     bounded only by the file size (which grows with conversation length).
//     Accepted: it is one keyed file read, never a directory walk, and only
//     legacy (<0.45) gemini-cli writes the format;
//   - gemini-cli >=0.45 JSONL (".jsonl" suffix) — first line is the metadata
//     object, subsequent lines are message records re-appended on update
//     plus {"$set":...} metadata patches. Scanned forward over the WHOLE
//     file (the same whole-file concession as the legacy path: gemini
//     re-appends the entire record on every update, so the final
//     token-bearing record of tool-using turns routinely exceeds any fixed
//     tail window) with last-occurrence-wins dedupe by message id, because
//     the recorder re-appends the final gemini message once its tokens
//     arrive. A non-nil {"$set":{"messages":[...]}} array replays
//     gemini-cli's loadConversationRecord semantics: accumulated usage is
//     cleared and the embedded messages are processed in array order.
//     {"$rewindTo":...} records are deliberately IGNORED here (unlike
//     ReadGeminiFile): rewound invocations still consumed the tokens they
//     report, so usage keeps counting them.
//
// Only messages with type "gemini" and a non-null tokens object are
// emitted. Mapping: InputTokens = input + tool - cached (cached prompt
// tokens are a subset of the prompt; clamped at zero), OutputTokens =
// output + thoughts, CacheReadTokens = cached, CacheCreationTokens = 0
// (gemini reports no cache-write tokens). EntryUUID and MessageID are both
// the message id. All-zero usage is skipped; malformed lines and metadata
// patch lines are tolerated silently.
func ExtractGeminiUsage(path string) ([]TailUsage, error) {
	if strings.HasSuffix(path, ".jsonl") {
		return extractGeminiUsageJSONL(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var conversation struct {
		Messages []geminiUsageMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &conversation); err != nil {
		return nil, err
	}
	var usages []TailUsage
	for _, msg := range conversation.Messages {
		if u, ok := geminiUsageFromMessage(msg); ok {
			usages = append(usages, u)
		}
	}
	return usages, nil
}

// extractGeminiUsageJSONL scans a JSONL chat recording forward in full,
// collapsing re-appended message records by id (last occurrence wins) and
// replaying non-nil {"$set":{"messages":[...]}} rewrites (clear, then
// process the embedded messages in order).
func extractGeminiUsageJSONL(path string) ([]TailUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, geminiScanInitialBuffer()), geminiScanBufferCap)

	var usages []TailUsage
	// byMessageID maps a message id to its index in usages so re-appended
	// records of one message collapse to a single entry.
	byMessageID := make(map[string]int)
	// record routes one message (top-level line or $set-embedded object)
	// through usage mapping + last-occurrence-wins dedup.
	record := func(msg geminiUsageMessage) {
		u, ok := geminiUsageFromMessage(msg)
		if !ok {
			return
		}
		if i, seen := byMessageID[u.MessageID]; seen {
			usages[i] = u
			return
		}
		byMessageID[u.MessageID] = len(usages)
		usages = append(usages, u)
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg geminiUsageMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.ID == "" {
			// Metadata object or {"$set":...} patch line. A non-nil
			// $set.messages array rewrote the whole conversation: replay it.
			var patch geminiSetPatch
			if err := json.Unmarshal(line, &patch); err != nil || patch.Set == nil || patch.Set.Messages == nil {
				continue
			}
			usages = nil
			byMessageID = make(map[string]int)
			for _, raw := range patch.Set.Messages {
				var embedded geminiUsageMessage
				if err := json.Unmarshal(raw, &embedded); err != nil {
					continue
				}
				record(embedded)
			}
			continue
		}
		record(msg)
	}
	// bufio.ErrTooLong is a soft stop: one oversized line (a
	// whole-conversation $set patch) must not fail the entire session, so
	// the usage parsed before it is returned. Any other scan error fails.
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return nil, err
	}
	return usages, nil
}

// geminiUsageFromMessage maps one recorded message to a TailUsage,
// reporting false for non-gemini messages, messages without tokens, and
// all-zero usage. Metadata records and {"$set":...} patch lines have no id
// and are rejected here as well.
func geminiUsageFromMessage(msg geminiUsageMessage) (TailUsage, bool) {
	if msg.ID == "" || msg.Type != "gemini" || msg.Tokens == nil {
		return TailUsage{}, false
	}
	input := msg.Tokens.Input + msg.Tokens.Tool - msg.Tokens.Cached
	if input < 0 {
		input = 0
	}
	u := TailUsage{
		EntryUUID:       msg.ID,
		MessageID:       msg.ID,
		Model:           msg.Model,
		InputTokens:     input,
		OutputTokens:    msg.Tokens.Output + msg.Tokens.Thoughts,
		CacheReadTokens: msg.Tokens.Cached,
	}
	if u.InputTokens <= 0 && u.OutputTokens <= 0 && u.CacheReadTokens <= 0 {
		return TailUsage{}, false
	}
	return u, true
}

// ExtractGeminiUsageFromSearchPaths reads gemini usage only after verifying
// path resolves under one of the merged gemini session roots (the defaults
// plus searchPaths). Mirrors ExtractTailUsageFromSearchPaths.
func ExtractGeminiUsageFromSearchPaths(searchPaths []string, path string) ([]TailUsage, error) {
	safePath, err := validateSearchPathFile(mergeGeminiSearchPaths(searchPaths), path)
	if err != nil {
		return nil, err
	}
	return ExtractGeminiUsage(safePath)
}
