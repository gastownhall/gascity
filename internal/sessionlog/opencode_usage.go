package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// openCodeUsageMessage is the usage-bearing slice of one OpenCode export
// message. Only assistant messages carry model, token and cost fields.
type openCodeUsageMessage struct {
	Info struct {
		ID         string   `json:"id"`
		Role       string   `json:"role"`
		ModelID    string   `json:"modelID"`
		ProviderID string   `json:"providerID"`
		Cost       *float64 `json:"cost"`
		Tokens     struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
		Time struct {
			Completed int64 `json:"completed"`
		} `json:"time"`
	} `json:"info"`
}

// ExtractOpenCodeTailUsage reads per-invocation token usage from an OpenCode
// session export and returns one TailUsage per completed assistant message, in
// file order. OpenCode mirrors a session as a single JSON document rather than
// an append-only log, so there is no tail window: the whole export is parsed
// and callers filter by their persisted cursor (the message id).
//
// A message that has not completed (no time.completed) or carries no tokens is
// skipped: OpenCode rewrites a message in place while it streams, so an early
// snapshot would record zeros under an identity the cursor then treats as
// settled. OpenCode's own cost is carried as ReportedCostUSD because it
// reflects the upstream (for example OpenRouter) routing the price table
// cannot know. An unparseable export is an error, never an empty result, so a
// torn mid-write mirror is retried rather than swept as usage-free.
func ExtractOpenCodeTailUsage(path string) ([]TailUsage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var export struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, err
	}
	var out []TailUsage
	for _, raw := range export.Messages {
		var m openCodeUsageMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		info := m.Info
		if strings.ToLower(strings.TrimSpace(info.Role)) != "assistant" || info.Time.Completed <= 0 {
			continue
		}
		id := strings.TrimSpace(info.ID)
		tokens := info.Tokens
		if id == "" || tokens.Input+tokens.Output+tokens.Reasoning+tokens.Cache.Read+tokens.Cache.Write == 0 {
			continue
		}
		usage := TailUsage{
			EntryUUID:           id,
			MessageID:           id,
			Model:               strings.TrimSpace(info.ModelID),
			InputTokens:         tokens.Input,
			OutputTokens:        tokens.Output,
			ReasoningTokens:     tokens.Reasoning,
			CacheReadTokens:     tokens.Cache.Read,
			CacheCreationTokens: tokens.Cache.Write,
			Timestamp:           time.UnixMilli(info.Time.Completed),
		}
		if info.Cost != nil {
			usage.ReportedCostUSD = *info.Cost
			usage.HasReportedCost = true
		}
		out = append(out, usage)
	}
	return out, nil
}

// ExtractOpenCodeTailUsageFromSearchPaths reads OpenCode usage only after
// verifying path resolves under one of the merged OpenCode export roots (the
// default mirror directory plus searchPaths).
func ExtractOpenCodeTailUsageFromSearchPaths(searchPaths []string, path string) ([]TailUsage, error) {
	safePath, err := validateSearchPathFile(mergeOpenCodeSearchPaths(searchPaths), path)
	if err != nil {
		return nil, err
	}
	return ExtractOpenCodeTailUsage(safePath)
}

// FindOpenCodeSessionFileByID resolves the export mirrored for one OpenCode
// session by its id (the bead's session_key, "ses_…"), which names the file.
// It is an identity lookup: unlike FindOpenCodeSessionFile it never falls back
// to the newest export in a shared working directory, so sessions that share a
// workdir cannot be attributed each other's transcript. It returns "" when the
// id is empty, is not a bare file name, or has no export yet.
func FindOpenCodeSessionFileByID(searchPaths []string, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || sessionID != filepath.Base(sessionID) || strings.ContainsAny(sessionID, `/\`) || sessionID == "." || sessionID == ".." {
		return ""
	}
	for _, root := range mergeOpenCodeSearchPaths(searchPaths) {
		if strings.TrimSpace(root) == "" {
			continue
		}
		candidate := filepath.Join(root, sessionID+".json")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}
