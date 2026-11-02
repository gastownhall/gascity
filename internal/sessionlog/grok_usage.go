package sessionlog

import (
	"encoding/json"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// Grok CLI usage layout (verified against 1.0.52 and 1.0.62). This is the
// CLI's own on-disk store under ~/.grok, not the ACP capture files
// grok_reader.go reads (those are found by session id in configured capture
// directories and carry no per-inference usage). Each gc wake starts a NEW
// grok session id
// (the builtin profile is keyless: ResumeFlag but no SessionIDFlag), and the
// CLI writes:
//
//	~/.grok/sessions/<url.PathEscape(cwd)>/<sid>/        (born at session start;
//	                                                      summary.json carries info.cwd)
//	~/.grok/logs/unified.jsonl                           (ONE shared log, every sid)
//
// The session directory holds no per-call token counts (newer CLIs add a
// per-turn usage.json and turn_completed updates, but only once a turn ends,
// so a turn interrupted by a gc stop never appears there); the per-inference
// usage lives in the shared log as
//
//	{"ts":"…","sid":"<uuidv7>","msg":"shell.turn.inference_done",
//	 "ctx":{"loop_index":1,"prompt_tokens":32807,"cached_prompt_tokens":1664,
//	        "completion_tokens":561,"reasoning_tokens":361,…}}
//
// and the model in effect for a sid comes from its {"msg":"model changed",
// "ctx":{"model":"grok-4.7"}} line. Verified on real logs: reasoning_tokens
// <= completion_tokens (reasoning is INCLUDED in completion, never added),
// cached_prompt_tokens <= prompt_tokens, and (sid, ts, loop_index) is unique.
// The shared log is the CLI's internal, undocumented diagnostics file: it
// does not keep full history (older lines are dropped), so inferences no
// longer in it are not recoverable, and its line format may change between
// releases.
const (
	grokSessionsDirName      = "sessions"
	grokUnifiedLogRelPath    = "logs/unified.jsonl"
	grokSessionSummaryFile   = "summary.json"
	grokMsgModelChanged      = "model changed"
	grokMsgTurnInferenceDone = "shell.turn.inference_done"
)

// DefaultGrokUsageRoots returns the root the grok usage extractor and session
// finder work under (~/.grok): the sessions tree and the shared unified log
// both hang off it. It is deliberately separate from DefaultGrokSearchPaths,
// which stays empty because the display-only ACP capture reader has no stable
// local transcript store to default to.
func DefaultGrokUsageRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".grok")}
}

func mergeGrokUsageRoots(extraPaths []string) []string {
	return mergePaths(DefaultGrokUsageRoots(), extraPaths)
}

// uuidV7CreationTime decodes the 48-bit Unix millisecond timestamp from a
// canonical-layout UUIDv7. Other UUID versions and malformed timestamp
// prefixes report false, so they never authorize a window match.
func uuidV7CreationTime(id string) (time.Time, bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || id[14] != '7' {
		return time.Time{}, false
	}
	timestampHex := id[:8] + id[9:13]
	millis, err := strconv.ParseUint(timestampHex, 16, 48)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(millis)), true
}

// grokSessionsDirForWorkDir is the per-cwd sessions directory the grok CLI
// writes under root for workDir.
func grokSessionsDirForWorkDir(root, workDir string) string {
	return filepath.Join(root, grokSessionsDirName, url.PathEscape(filepath.Clean(workDir)))
}

// FindGrokSessionDirNear resolves the grok session directory a gc wake
// started: the UUIDv7-named entry under <root>/sessions/<escaped cwd>/ whose
// embedded creation time falls in [anchor-1m, anchor+window], confirmed
// against summary.json's info.cwd when that file is readable. Exactly one
// match is returned; several are refused as ambiguous (two wakes in one cwd
// inside the window cannot be told apart) and none, a zero anchor, an empty
// workDir or a non-positive window return "". It is the string-only wrapper
// of FindGrokSessionDirNearScan, mirroring FindCodexSessionFileNear.
func FindGrokSessionDirNear(searchPaths []string, workDir string, anchor time.Time, window time.Duration) string {
	path, _ := FindGrokSessionDirNearScan(searchPaths, workDir, anchor, window)
	return path
}

// FindGrokSessionDirNearScan is FindGrokSessionDirNear with a clean-scan
// signal, under the same contract as FindCodexSessionFileNearScan: scanClean
// is false when a readdir or summary probe failed with a non-ENOENT IO fault,
// so a lone hit or an empty result is non-definitive and the keyless sweep
// retries rather than settling. An ambiguity refusal (>1 match) is definitive
// and returns ("", true) regardless of scan noise. Bad inputs return
// ("", true) — a clean no-op, not a fault.
func FindGrokSessionDirNearScan(searchPaths []string, workDir string, anchor time.Time, window time.Duration) (string, bool) {
	if strings.TrimSpace(workDir) == "" || anchor.IsZero() || window <= 0 {
		return "", true
	}
	start := anchor.Add(-time.Minute)
	end := anchor.Add(window)
	var matches []string
	seen := make(map[string]bool)
	dirty := false
	for _, root := range mergeGrokUsageRoots(searchPaths) {
		dir := grokSessionsDirForWorkDir(root, workDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			// No sessions for this cwd under this root is the normal case and
			// stays clean; anything else (EMFILE/ESTALE/ENOTDIR) clouds the scan.
			if !os.IsNotExist(err) {
				dirty = true
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			created, ok := uuidV7CreationTime(e.Name())
			if !ok || created.Before(start) || created.After(end) {
				continue
			}
			sessionDir := filepath.Join(dir, e.Name())
			match, clean := grokSessionCWDMatchesScan(sessionDir, workDir)
			if !clean {
				dirty = true
			}
			if !match {
				continue
			}
			appendCodexRolloutMatch(sessionDir, seen, &matches)
			if len(matches) > 1 {
				return "", true // ambiguous: a definitive refusal, independent of scan noise
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], !dirty
	}
	return "", !dirty
}

// grokSessionSummary is the subset of a grok session's summary.json the
// finder confirms cwd against.
type grokSessionSummary struct {
	Info struct {
		CWD string `json:"cwd"`
	} `json:"info"`
}

// grokSessionCWDMatchesScan confirms sessionDir belongs to workDir via its
// summary.json info.cwd. The directory name already encodes the cwd, so a
// missing or unparseable summary (the file is written late in the session)
// keeps the match; only a recorded cwd that names a DIFFERENT directory
// rejects it. clean is false on a non-ENOENT read fault.
func grokSessionCWDMatchesScan(sessionDir, workDir string) (match bool, clean bool) {
	data, err := os.ReadFile(filepath.Join(sessionDir, grokSessionSummaryFile))
	if err != nil {
		if os.IsNotExist(err) {
			return true, true
		}
		return false, false
	}
	var summary grokSessionSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return true, true
	}
	cwd := strings.TrimSpace(summary.Info.CWD)
	if cwd == "" {
		return true, true
	}
	return pathutil.SamePath(cwd, workDir), true
}

// grokLogEntry is the minimal shape decoded from each unified.jsonl line.
type grokLogEntry struct {
	TS  string          `json:"ts"`
	SID string          `json:"sid"`
	Msg string          `json:"msg"`
	Ctx json.RawMessage `json:"ctx"`
}

type grokModelChangedCtx struct {
	Model string `json:"model"`
}

type grokInferenceDoneCtx struct {
	LoopIndex          int `json:"loop_index"`
	PromptTokens       int `json:"prompt_tokens"`
	CachedPromptTokens int `json:"cached_prompt_tokens"`
	CompletionTokens   int `json:"completion_tokens"`
	ReasoningTokens    int `json:"reasoning_tokens"`
}

// ExtractGrokTailUsage returns one TailUsage per completed grok inference of
// the session sessionDir names (its base name is the sid), read from the
// shared unified log <root>/logs/unified.jsonl where root is three levels up
// (<root>/sessions/<escaped cwd>/<sid>).
//
// The scan window grows from tailChunkSize, doubling, until the cursor entry
// is in view, the sid's "model changed" line is in view (it precedes every
// inference of the session, so the whole session is then covered), the whole
// file is in view, or maxUsageScanBytes is reached — the cap bounds a
// cold-cursor read of the shared log to 16MB and is logged like the claude
// extractor's. Lines of other sids are skipped; malformed lines are
// tolerated; an inference with no model change in view records Model ""
// (unpriced, like a codex seed miss) rather than a guess.
//
// Token mapping: InputTokens = prompt_tokens − cached_prompt_tokens (clamped
// at 0), CacheReadTokens = cached_prompt_tokens, OutputTokens =
// completion_tokens, ReasoningTokens = reasoning_tokens (a subset of
// completion, not added), CacheCreationTokens 0. EntryUUID is the line ts and
// MessageID is "grok:<ts>:<loop_index>", unique per (sid, ts, loop_index);
// all-zero usage is skipped. Callers may receive entries at or before the
// cursor and must still filter.
func ExtractGrokTailUsage(sessionDir, cursorID string) ([]TailUsage, error) {
	sid, logPath, err := grokUnifiedLogForSession(sessionDir)
	if err != nil {
		return nil, err
	}
	return extractGrokTailUsage(logPath, sid, cursorID, maxUsageScanBytes)
}

// grokUnifiedLogForSession derives the session id and the shared log path
// from a session directory.
func grokUnifiedLogForSession(sessionDir string) (sid, logPath string, err error) {
	clean := filepath.Clean(strings.TrimSpace(sessionDir))
	sid = filepath.Base(clean)
	if sid == "" || sid == "." || sid == string(filepath.Separator) {
		return "", "", os.ErrNotExist
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(clean)))
	return sid, filepath.Join(root, filepath.FromSlash(grokUnifiedLogRelPath)), nil
}

// extractGrokTailUsage is ExtractGrokTailUsage with the growth cap supplied by
// the caller, so tests can drive the cap branch without a multi-megabyte log.
func extractGrokTailUsage(logPath, sid, cursorID string, maxScanBytes int64) ([]TailUsage, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	for window := int64(tailChunkSize); ; window *= 2 {
		data, _, truncated, err := readTailWindow(f, window)
		if err != nil {
			return nil, err
		}
		lines, err := splitLines(data)
		if err != nil {
			return nil, err
		}
		usages, sawModelChange := parseGrokUsageLines(lines, sid)
		if !truncated || sawModelChange || (cursorID != "" && containsCursor(usages, cursorID)) {
			return usages, nil
		}
		if window >= maxScanBytes {
			log.Printf("sessionlog: grok usage scan cap reached path=%q sid=%q window_bytes=%d cursor=%q; inferences older than the window are not returned",
				logPath, sid, window, cursorID)
			return usages, nil
		}
	}
}

// parseGrokUsageLines converts the sid's inference_done lines in a window to
// TailUsage, labeling each with the model named by the sid's latest preceding
// "model changed" line in the window. sawModelChange reports whether such a
// line was in view, the extractor's whole-session stop condition.
func parseGrokUsageLines(lines [][]byte, sid string) ([]TailUsage, bool) {
	var usages []TailUsage
	model := ""
	sawModelChange := false
	for _, line := range lines {
		var entry grokLogEntry
		if err := json.Unmarshal(line, &entry); err != nil || entry.SID != sid {
			continue
		}
		switch entry.Msg {
		case grokMsgModelChanged:
			var ctx grokModelChangedCtx
			if err := json.Unmarshal(entry.Ctx, &ctx); err != nil {
				continue
			}
			sawModelChange = true
			if m := strings.TrimSpace(ctx.Model); m != "" {
				model = m
			}
		case grokMsgTurnInferenceDone:
			var ctx grokInferenceDoneCtx
			if err := json.Unmarshal(entry.Ctx, &ctx); err != nil {
				continue
			}
			input := ctx.PromptTokens - ctx.CachedPromptTokens
			if input < 0 {
				input = 0
			}
			u := TailUsage{
				EntryUUID:       entry.TS,
				MessageID:       "grok:" + entry.TS + ":" + strconv.Itoa(ctx.LoopIndex),
				Model:           model,
				InputTokens:     input,
				OutputTokens:    ctx.CompletionTokens,
				ReasoningTokens: ctx.ReasoningTokens,
				CacheReadTokens: ctx.CachedPromptTokens,
				Timestamp:       parseGrokLogTime(entry.TS),
			}
			if u.InputTokens <= 0 && u.OutputTokens <= 0 && u.ReasoningTokens <= 0 && u.CacheReadTokens <= 0 {
				continue
			}
			usages = append(usages, u)
		}
	}
	return usages, sawModelChange
}

// parseGrokLogTime parses a unified.jsonl ts (RFC3339Nano, falling back to
// RFC3339); zero when unparseable.
func parseGrokLogTime(ts string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t
	}
	return time.Time{}
}

// ExtractGrokTailUsageFromSearchPaths reads grok usage only after verifying
// the session directory AND the derived shared log both resolve under one of
// the merged grok roots (DefaultGrokUsageRoots plus searchPaths). Mirrors
// ExtractCodexTailUsageFromSearchPaths.
func ExtractGrokTailUsageFromSearchPaths(searchPaths []string, sessionDir, cursorID string) ([]TailUsage, error) {
	roots := mergeGrokUsageRoots(searchPaths)
	safeDir, err := validateSearchPathFile(roots, sessionDir)
	if err != nil {
		return nil, err
	}
	sid, logPath, err := grokUnifiedLogForSession(safeDir)
	if err != nil {
		return nil, err
	}
	safeLog, err := validateSearchPathFile(roots, logPath)
	if err != nil {
		return nil, err
	}
	return extractGrokTailUsage(safeLog, sid, cursorID, maxUsageScanBytes)
}
