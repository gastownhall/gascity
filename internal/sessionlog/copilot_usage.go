package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// copilotUsageRecord is the usage-bearing slice of one GitHub Copilot CLI
// session.usage_record event. Copilot writes one such event per model call.
type copilotUsageRecord struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Data      struct {
		Usage struct {
			Model            string `json:"model"`
			APICallID        string `json:"apiCallId"`
			InputTokens      int    `json:"inputTokens"`
			OutputTokens     int    `json:"outputTokens"`
			CacheReadTokens  int    `json:"cacheReadTokens"`
			CacheWriteTokens int    `json:"cacheWriteTokens"`
			ReasoningTokens  int    `json:"reasoningTokens"`
		} `json:"usage"`
	} `json:"data"`
}

// ExtractCopilotTailUsage reads per-invocation token usage from a GitHub
// Copilot CLI session-state events.jsonl file and returns one TailUsage per
// session.usage_record event, in file order. With a cursor, the scan window
// grows from the fixed tail until cursorID is in view, exactly as
// ExtractTailUsageSince does for Claude transcripts. Without one (the first
// read of a session) the whole log is read, up to maxUsageScanBytes, instead
// of only the fixed tail: each Copilot CLI process writes its own
// events.jsonl, so nothing in it belongs to another session, and its tool
// output makes logs large (a median of about 150KB in a live city), so a 64KB
// first pass dropped over a third of the calls of a session swept once at its
// end.
//
// Mapping (checked against real Copilot CLI 1.0.x session logs, where
// inputTokens >= cacheReadTokens + cacheWriteTokens on every record):
//
//   - InputTokens = inputTokens - cacheReadTokens - cacheWriteTokens (Copilot's
//     inputTokens is the whole prompt; clamped at zero)
//   - CacheReadTokens = cacheReadTokens
//   - CacheCreationTokens = cacheWriteTokens
//   - OutputTokens = outputTokens
//   - ReasoningTokens = reasoningTokens (reported separately; never larger
//     than outputTokens in observed logs, so it is not added to output)
//
// MessageID is the record's apiCallId, the provider call id, so a call is
// counted once even when the record is read on two sweeps; EntryUUID is the
// event id. The record's "cost" field is a premium-request multiplier, not a
// currency amount, so it is not carried as ReportedCostUSD: the fact is priced
// from the registry or flagged Unpriced.
func ExtractCopilotTailUsage(path, cursorID string) ([]TailUsage, error) {
	if cursorID != "" {
		return extractTailUsageSinceWith(path, cursorID, maxUsageScanBytes, parseCopilotTailUsage)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file
	data, _, _, err := readTailWindow(f, maxUsageScanBytes)
	if err != nil {
		return nil, err
	}
	return parseCopilotTailUsage(data)
}

// ExtractCopilotTailUsageFromSearchPaths reads Copilot usage only after
// verifying path resolves under one of the merged Copilot session-state roots
// (the default ~/.copilot/session-state plus searchPaths).
func ExtractCopilotTailUsageFromSearchPaths(searchPaths []string, path, cursorID string) ([]TailUsage, error) {
	safePath, err := validateSearchPathFile(mergeCopilotSearchPaths(searchPaths), path)
	if err != nil {
		return nil, err
	}
	return ExtractCopilotTailUsage(safePath, cursorID)
}

func parseCopilotTailUsage(data []byte) ([]TailUsage, error) {
	lines, err := splitLines(data)
	if err != nil {
		return nil, err
	}
	var usages []TailUsage
	byMessageID := make(map[string]int)
	for _, line := range lines {
		if !bytes.Contains(line, []byte(`"session.usage_record"`)) {
			continue
		}
		var rec copilotUsageRecord
		if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "session.usage_record" {
			continue
		}
		u := rec.Data.Usage
		id := strings.TrimSpace(rec.ID)
		if id == "" {
			continue
		}
		input := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
		if input < 0 {
			input = 0
		}
		usage := TailUsage{
			EntryUUID:           id,
			MessageID:           strings.TrimSpace(u.APICallID),
			Model:               strings.TrimSpace(u.Model),
			InputTokens:         input,
			OutputTokens:        u.OutputTokens,
			ReasoningTokens:     u.ReasoningTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheCreationTokens: u.CacheWriteTokens,
		}
		if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(rec.Timestamp)); err == nil {
			usage.Timestamp = ts
		}
		if usage.InputTokens <= 0 && usage.OutputTokens <= 0 && usage.CacheReadTokens <= 0 && usage.CacheCreationTokens <= 0 {
			continue
		}
		if usage.MessageID != "" {
			if i, seen := byMessageID[usage.MessageID]; seen {
				usages[i] = usage
				continue
			}
			byMessageID[usage.MessageID] = len(usages)
		}
		usages = append(usages, usage)
	}
	return usages, nil
}

// FindCopilotSessionFileNearScan resolves the Copilot session-state
// events.jsonl for a session that has no provider session id: the one whose
// workspace cwd is workDir and whose session.start time falls in
// [anchor-1m, anchor+window]. It is the Copilot twin of
// FindCodexSessionFileNearScan and returns the same clean-scan signal.
//
// Copilot pool seats reuse one work dir across many sessions, so the newest
// file for the work dir can be an earlier session's log. The start-time window
// is what ties the file to this wake. More than one match is refused as
// ambiguous (""), never guessed. Session directories whose events.jsonl was
// last written before the window opened are skipped without being opened,
// which keeps the scan cheap on a large session-state directory.
func FindCopilotSessionFileNearScan(searchPaths []string, workDir string, anchor time.Time, window time.Duration) (string, bool) {
	if strings.TrimSpace(workDir) == "" || anchor.IsZero() || window <= 0 {
		return "", true
	}
	start := anchor.Add(-time.Minute)
	end := anchor.Add(window)
	var matches []string
	seen := make(map[string]bool)
	dirty := false
	for _, root := range mergeCopilotSearchPaths(searchPaths) {
		entries, err := os.ReadDir(root)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				dirty = true
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(root, entry.Name(), "events.jsonl")
			info, err := os.Stat(path)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					dirty = true
				}
				continue
			}
			if info.IsDir() || info.ModTime().Before(start) {
				continue
			}
			started, cwd, ok := copilotSessionStart(path)
			if !ok {
				continue
			}
			if started.Before(start) || started.After(end) || !copilotCWDMatches(cwd, path, workDir) {
				continue
			}
			key := path
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				key = resolved
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			matches = append(matches, path)
			if len(matches) > 1 {
				return "", true
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], !dirty
	}
	return "", !dirty
}

func copilotCWDMatches(cwd, path, workDir string) bool {
	if cwd == "" {
		return copilotSessionCWDMatches(path, workDir)
	}
	return pathutil.SamePath(cwd, workDir)
}

// copilotSessionStart reads the session.start event at the head of a Copilot
// events.jsonl and returns its start time and cwd.
func copilotSessionStart(path string) (time.Time, string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, "", false
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for i := 0; i < 8 && scanner.Scan(); i++ {
		var event struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Data      struct {
				StartTime string `json:"startTime"`
				Context   struct {
					CWD string `json:"cwd"`
				} `json:"context"`
			} `json:"data"`
		}
		if json.Unmarshal(bytes.TrimSpace(scanner.Bytes()), &event) != nil || event.Type != "session.start" {
			continue
		}
		raw := strings.TrimSpace(event.Data.StartTime)
		if raw == "" {
			raw = strings.TrimSpace(event.Timestamp)
		}
		started, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return time.Time{}, "", false
		}
		return started, strings.TrimSpace(event.Data.Context.CWD), true
	}
	return time.Time{}, "", false
}
