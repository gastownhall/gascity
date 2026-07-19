package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// Session is the resolved view of a Claude JSONL session file.
type Session struct {
	// ID is the session identifier (from the filename).
	ID string

	// Messages is the active branch in conversation order (root → tip).
	// Entries that aren't relevant for display (file-history-snapshot,
	// progress hooks) are filtered out.
	Messages []*Entry

	// OrphanedToolUseIDs contains tool_use IDs with no matching result.
	OrphanedToolUseIDs map[string]bool

	// HasBranches is true if the session has conversation forks.
	HasBranches bool

	// Pagination metadata.
	Pagination *PaginationInfo

	// Diagnostics surfaces parser health for the underlying session file.
	Diagnostics SessionDiagnostics
}

// SessionDiagnostics reports non-fatal issues detected while loading a
// session file.
type SessionDiagnostics struct {
	MalformedLineCount int
	MalformedTail      bool
}

// PaginationInfo describes the pagination state of a session response.
type PaginationInfo struct {
	HasOlderMessages       bool   `json:"has_older_messages"`
	TotalMessageCount      int    `json:"total_message_count"`
	ReturnedMessageCount   int    `json:"returned_message_count"`
	TruncatedBeforeMessage string `json:"truncated_before_message,omitempty"`
	TotalCompactions       int    `json:"total_compactions"`
}

// RawPayloads decodes each non-empty Entry.Raw into a generic JSON value
// (map[string]any for objects, []any for arrays, etc.) and returns the
// slice. Used by API response builders so handlers can emit the
// provider-native transcript frames as typed `any` fields without
// touching json.RawMessage in the API layer.
//
// Deprecated: prefer RawPayloadBytes when the downstream consumer will
// marshal-and-ship the result. The Unmarshal→`any`→Marshal round-trip
// loses int64 precision above 2^53 (tool-call IDs, nanosecond
// timestamps) and does not preserve map-key order. Kept for the small
// number of callers that actually consume the decoded form.
func (s *Session) RawPayloads() []any {
	out := make([]any, 0, len(s.Messages))
	for _, entry := range s.Messages {
		if entry == nil || len(entry.Raw) == 0 {
			continue
		}
		var v any
		if err := json.Unmarshal(entry.Raw, &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// RawPayloadBytes returns the raw JSON bytes for each non-empty
// Entry.Raw. Each returned slice is a defensive copy — callers can
// append/modify freely without corrupting the underlying Session.
// Prefer this over RawPayloads when the data is about to be emitted
// on the wire (SSE streams, API responses), because it preserves
// byte-identity, int64 precision, and map-key order.
func (s *Session) RawPayloadBytes() []json.RawMessage {
	out := make([]json.RawMessage, 0, len(s.Messages))
	for _, entry := range s.Messages {
		if entry == nil || len(entry.Raw) == 0 {
			continue
		}
		if !json.Valid(entry.Raw) {
			continue
		}
		out = append(out, append(json.RawMessage(nil), entry.Raw...))
	}
	return out
}

// displayTypes are entry types included in the display output.
var displayTypes = map[string]bool{
	"user":      true,
	"assistant": true,
	"system":    true,
	"result":    true,
}

// ReadFile reads a Claude JSONL session file and resolves it into a
// Session. The file is parsed, DAG-resolved, and filtered to display
// entries. Returns the most recent tailCompactions worth of messages
// (0 = all messages).
func ReadFile(path string, tailCompactions int) (*Session, error) {
	entries, diagnostics, err := parseFileDetailed(path)
	if err != nil {
		return nil, err
	}

	dag := BuildDag(entries)

	// Filter to display types.
	var messages []*Entry
	for _, e := range dag.ActiveBranch {
		if displayTypes[e.Type] {
			messages = append(messages, e)
		}
	}

	// Extract session ID from filename.
	base := filepath.Base(path)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))

	sess := &Session{
		ID:                 sessionID,
		Messages:           messages,
		OrphanedToolUseIDs: dag.OrphanedToolUseIDs,
		HasBranches:        dag.HasBranches,
		Diagnostics:        diagnostics,
	}

	// Apply compact-boundary pagination.
	if tailCompactions > 0 {
		paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, "", "")
		sess.Messages = paginated
		sess.Pagination = info
	}

	return sess, nil
}

// ReadProviderFile reads a provider-specific transcript file.
func ReadProviderFile(provider, path string, tailCompactions int) (*Session, error) {
	switch ProviderFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
	case "kimi":
		return ReadKimiFile(path, tailCompactions)
	case "mimocode":
		return ReadMimoCodeFile(path, tailCompactions)
	case "opencode":
		return ReadOpenCodeFile(path, tailCompactions)
	case "pi":
		return ReadPiFile(path, tailCompactions)
	case "antigravity":
		return ReadAntigravityFile(path, tailCompactions)
	default:
		return ReadFile(path, tailCompactions)
	}
}

// ReadFileRaw reads a session file without display-type filtering.
// All DAG-resolved entries are returned, preserving tool_use, progress,
// and other non-display types. Used by the raw transcript API.
func ReadFileRaw(path string, tailCompactions int) (*Session, error) {
	entries, diagnostics, err := parseFileDetailed(path)
	if err != nil {
		return nil, err
	}

	dag := BuildDag(entries)
	messages := dag.ActiveBranch

	base := filepath.Base(path)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))

	sess := &Session{
		ID:                 sessionID,
		Messages:           messages,
		OrphanedToolUseIDs: dag.OrphanedToolUseIDs,
		HasBranches:        dag.HasBranches,
		Diagnostics:        diagnostics,
	}

	if tailCompactions > 0 {
		paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, "", "")
		sess.Messages = paginated
		sess.Pagination = info
	}

	return sess, nil
}

// ReadProviderFileRaw reads a provider-specific transcript file without
// display-type filtering. For Codex, the raw JSONL lines are already preserved
// on each returned entry, so the Codex reader is sufficient for both raw and
// conversation views.
func ReadProviderFileRaw(provider, path string, tailCompactions int) (*Session, error) {
	switch ProviderFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
	case "kimi":
		return ReadKimiFile(path, tailCompactions)
	case "mimocode":
		return ReadMimoCodeFile(path, tailCompactions)
	case "opencode":
		return ReadOpenCodeFile(path, tailCompactions)
	case "pi":
		return ReadPiFile(path, tailCompactions)
	case "antigravity":
		return ReadAntigravityFileRaw(path, tailCompactions)
	default:
		return ReadFileRaw(path, tailCompactions)
	}
}

// ReadFileOlder loads older messages before a cursor, returning the
// previous tailCompactions segment.
func ReadFileOlder(path string, tailCompactions int, beforeMessageID string) (*Session, error) {
	entries, diagnostics, err := parseFileDetailed(path)
	if err != nil {
		return nil, err
	}

	dag := BuildDag(entries)

	var messages []*Entry
	for _, e := range dag.ActiveBranch {
		if displayTypes[e.Type] {
			messages = append(messages, e)
		}
	}

	base := filepath.Base(path)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))

	paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, beforeMessageID, "")

	return &Session{
		ID:                 sessionID,
		Messages:           paginated,
		OrphanedToolUseIDs: dag.OrphanedToolUseIDs,
		HasBranches:        dag.HasBranches,
		Pagination:         info,
		Diagnostics:        diagnostics,
	}, nil
}

// ReadFileRawOlder loads older raw (unfiltered) messages before a cursor.
func ReadFileRawOlder(path string, tailCompactions int, beforeMessageID string) (*Session, error) {
	entries, diagnostics, err := parseFileDetailed(path)
	if err != nil {
		return nil, err
	}

	dag := BuildDag(entries)
	messages := dag.ActiveBranch

	base := filepath.Base(path)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))

	paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, beforeMessageID, "")

	return &Session{
		ID:                 sessionID,
		Messages:           paginated,
		OrphanedToolUseIDs: dag.OrphanedToolUseIDs,
		HasBranches:        dag.HasBranches,
		Pagination:         info,
		Diagnostics:        diagnostics,
	}, nil
}

// ReadProviderFileOlder reads an older page of a provider-specific transcript.
// Provider families without page-aware readers return the full provider
// transcript.
func ReadProviderFileOlder(provider, path string, tailCompactions int, beforeMessageID string) (*Session, error) {
	switch ProviderFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
	case "kimi":
		return ReadKimiFilePage(path, tailCompactions, beforeMessageID, "")
	case "mimocode":
		return ReadMimoCodeFile(path, tailCompactions)
	case "opencode":
		return ReadOpenCodeFile(path, tailCompactions)
	case "pi":
		return ReadPiFile(path, tailCompactions)
	case "antigravity":
		return ReadAntigravityFilePage(path, tailCompactions, beforeMessageID, "")
	default:
		return ReadFileOlder(path, tailCompactions, beforeMessageID)
	}
}

// ReadProviderFileRawOlder reads an older page of a provider-specific raw
// transcript. Provider families without page-aware readers return the full
// provider transcript.
func ReadProviderFileRawOlder(provider, path string, tailCompactions int, beforeMessageID string) (*Session, error) {
	switch ProviderFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
	case "kimi":
		return ReadKimiFilePage(path, tailCompactions, beforeMessageID, "")
	case "mimocode":
		return ReadMimoCodeFile(path, tailCompactions)
	case "opencode":
		return ReadOpenCodeFile(path, tailCompactions)
	case "pi":
		return ReadPiFile(path, tailCompactions)
	case "antigravity":
		return ReadAntigravityFileRawPage(path, tailCompactions, beforeMessageID, "")
	default:
		return ReadFileRawOlder(path, tailCompactions, beforeMessageID)
	}
}

// ReadFileNewer loads newer messages after a cursor.
func ReadFileNewer(path string, tailCompactions int, afterMessageID string) (*Session, error) {
	entries, diagnostics, err := parseFileDetailed(path)
	if err != nil {
		return nil, err
	}

	dag := BuildDag(entries)

	var messages []*Entry
	for _, e := range dag.ActiveBranch {
		if displayTypes[e.Type] {
			messages = append(messages, e)
		}
	}

	base := filepath.Base(path)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))

	paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, "", afterMessageID)

	return &Session{
		ID:                 sessionID,
		Messages:           paginated,
		OrphanedToolUseIDs: dag.OrphanedToolUseIDs,
		HasBranches:        dag.HasBranches,
		Pagination:         info,
		Diagnostics:        diagnostics,
	}, nil
}

// ReadFileRawNewer loads newer raw (unfiltered) messages after a cursor.
func ReadFileRawNewer(path string, tailCompactions int, afterMessageID string) (*Session, error) {
	entries, diagnostics, err := parseFileDetailed(path)
	if err != nil {
		return nil, err
	}

	dag := BuildDag(entries)
	messages := dag.ActiveBranch

	base := filepath.Base(path)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))

	paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, "", afterMessageID)

	return &Session{
		ID:                 sessionID,
		Messages:           paginated,
		OrphanedToolUseIDs: dag.OrphanedToolUseIDs,
		HasBranches:        dag.HasBranches,
		Pagination:         info,
		Diagnostics:        diagnostics,
	}, nil
}

// ReadProviderFileNewer reads a newer page of a provider-specific transcript.
// Provider families without page-aware readers return the full provider
// transcript.
func ReadProviderFileNewer(provider, path string, tailCompactions int, afterMessageID string) (*Session, error) {
	switch ProviderFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
	case "kimi":
		return ReadKimiFilePage(path, tailCompactions, "", afterMessageID)
	case "mimocode":
		return ReadMimoCodeFile(path, tailCompactions)
	case "opencode":
		return ReadOpenCodeFile(path, tailCompactions)
	case "pi":
		return ReadPiFile(path, tailCompactions)
	case "antigravity":
		return ReadAntigravityFilePage(path, tailCompactions, "", afterMessageID)
	default:
		return ReadFileNewer(path, tailCompactions, afterMessageID)
	}
}

// ReadProviderFileRawNewer reads a newer page of a provider-specific raw
// transcript. Provider families without page-aware readers return the full
// provider transcript.
func ReadProviderFileRawNewer(provider, path string, tailCompactions int, afterMessageID string) (*Session, error) {
	switch ProviderFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
	case "kimi":
		return ReadKimiFilePage(path, tailCompactions, "", afterMessageID)
	case "mimocode":
		return ReadMimoCodeFile(path, tailCompactions)
	case "opencode":
		return ReadOpenCodeFile(path, tailCompactions)
	case "pi":
		return ReadPiFile(path, tailCompactions)
	case "antigravity":
		return ReadAntigravityFileRawPage(path, tailCompactions, "", afterMessageID)
	default:
		return ReadFileRawNewer(path, tailCompactions, afterMessageID)
	}
}

// parseFile reads all JSONL lines from a file into entries.
func parseFile(path string) ([]*Entry, error) {
	entries, _, err := parseFileDetailed(path)
	return entries, err
}

// parseFileDetailed reads all JSONL lines from a file into entries and
// returns load diagnostics for malformed lines and torn tails.
func parseFileDetailed(path string) ([]*Entry, SessionDiagnostics, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, SessionDiagnostics{}, fmt.Errorf("opening session file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	var entries []*Entry
	var diagnostics SessionDiagnostics
	var lastNonEmptyLineMalformed bool
	scanner := bufio.NewScanner(f)
	// Default scanner buffer is 64KB; Claude entries can be large
	// (tool results with full file contents, base64 images, etc.).
	// Use 50MB max to handle very large entries without aborting the whole file.
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue // skip malformed lines
		}
		lastNonEmptyLineMalformed = false
		// Preserve the raw JSON for API pass-through.
		raw := make([]byte, len(line))
		copy(raw, line)
		e.Raw = raw
		entries = append(entries, &e)
	}
	if err := scanner.Err(); err != nil {
		return nil, SessionDiagnostics{}, fmt.Errorf("scanning session file: %w", err)
	}

	diagnostics.MalformedTail = lastNonEmptyLineMalformed
	return entries, diagnostics, nil
}

// sliceAtCompactBoundaries returns the tail portion of messages starting
// from the Nth-from-last compact boundary. The boundary itself is
// included so consumers can render a "Context compacted" divider.
func sliceAtCompactBoundaries(messages []*Entry, tailCompactions int, beforeMessageID, afterMessageID string) ([]*Entry, *PaginationInfo) {
	totalCount := len(messages)

	// For "load older" requests: truncate at cursor first.
	working := messages
	if beforeMessageID != "" {
		for i, m := range messages {
			if m.UUID == beforeMessageID {
				working = messages[:i]
				break
			}
		}
	}

	// For "load newer" requests: truncate at cursor, keeping entries after it.
	if afterMessageID != "" {
		for i, m := range working {
			if m.UUID == afterMessageID {
				working = working[i+1:]
				break
			}
		}
	}

	// Guard: tailCompactions <= 0 means "return the working set as-is".
	if tailCompactions <= 0 {
		return working, &PaginationInfo{
			HasOlderMessages:     false,
			TotalMessageCount:    totalCount,
			ReturnedMessageCount: len(working),
		}
	}

	// Find all compact_boundary indices.
	var compactIndices []int
	for i, m := range working {
		if m.IsCompactBoundary() {
			compactIndices = append(compactIndices, i)
		}
	}

	totalCompactions := len(compactIndices)

	// Fewer boundaries than requested — return everything.
	if len(compactIndices) <= tailCompactions {
		return working, &PaginationInfo{
			HasOlderMessages:     false,
			TotalMessageCount:    totalCount,
			ReturnedMessageCount: len(working),
			TotalCompactions:     totalCompactions,
		}
	}

	// Slice from the Nth-from-last boundary (inclusive).
	sliceFrom := compactIndices[len(compactIndices)-tailCompactions]
	sliced := working[sliceFrom:]

	var truncatedBefore string
	if len(sliced) > 0 {
		truncatedBefore = sliced[0].UUID
	}

	return sliced, &PaginationInfo{
		HasOlderMessages:       true,
		TotalMessageCount:      totalCount,
		ReturnedMessageCount:   len(sliced),
		TruncatedBeforeMessage: truncatedBefore,
		TotalCompactions:       totalCompactions,
	}
}

// FindSessionFile searches for the most recently modified JSONL session file
// matching the given working directory. It tries slug-based lookup (Claude)
// across all search paths, then falls back to CWD-based providers that do not
// expose stable IDs for generic auto lookup.
func FindSessionFile(searchPaths []string, workDir string) string {
	// Try slug-based lookup first (Claude: {searchPath}/{slug}/*.jsonl).
	if path := findSlugSessionFile(searchPaths, workDir); path != "" {
		return path
	}
	// Fall back to Codex CWD-based lookup.
	if path := FindCodexSessionFile(searchPaths, workDir); path != "" {
		return path
	}
	return FindPiSessionFile(searchPaths, workDir)
}

// FindSessionFileForProvider resolves the best available transcript file for a
// specific provider.
func FindSessionFileForProvider(searchPaths []string, provider, workDir string) string {
	switch ProviderFamily(provider) {
	case "codex":
		return FindCodexSessionFile(searchPaths, workDir)
	case "gemini":
		return FindGeminiSessionFile(searchPaths, workDir)
	case "kimi":
		return FindKimiSessionFile(searchPaths, workDir)
	case "mimocode":
		return FindMimoCodeSessionFile(searchPaths, workDir)
	case "opencode":
		return FindOpenCodeSessionFile(searchPaths, workDir)
	case "pi":
		return FindPiSessionFile(searchPaths, workDir)
	case "antigravity":
		return FindAntigravitySessionFile(searchPaths, workDir)
	case "", "auto":
		return FindSessionFile(searchPaths, workDir)
	default:
		return findSlugSessionFile(searchPaths, workDir)
	}
}

// FindProviderFallbackSessionFile resolves the narrower provider-specific
// fallback path to use when a keyed transcript lookup misses. This avoids
// silently jumping to an unrelated transcript that merely shares the same
// workdir while still allowing canonical provider fallback files.
func FindProviderFallbackSessionFile(searchPaths []string, provider, workDir string) string {
	switch ProviderFamily(provider) {
	case "codex":
		return FindCodexSessionFile(searchPaths, workDir)
	case "gemini":
		return FindGeminiSessionFile(searchPaths, workDir)
	case "kimi":
		return FindKimiSessionFile(searchPaths, workDir)
	case "mimocode":
		return FindMimoCodeSessionFile(searchPaths, workDir)
	case "opencode":
		return FindOpenCodeSessionFile(searchPaths, workDir)
	case "pi":
		return FindPiSessionFile(searchPaths, workDir)
	case "antigravity":
		return FindAntigravitySessionFile(searchPaths, workDir)
	default:
		return findClaudeLatestSessionFile(searchPaths, workDir)
	}
}

// FindSessionFileByID resolves a Claude-style session log path using the
// known session ID. This is the safest lookup when multiple sessions share
// the same working directory.
func FindSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	if workDir == "" || sessionID == "" {
		return ""
	}
	fileName := safeSessionLogFileName(sessionID)
	if fileName == "" {
		return ""
	}
	return findSessionFileByIDForCandidates(searchPaths, claudeProjectSlugCandidates(workDir), fileName)
}

func findSessionFileByIDForCandidates(searchPaths, slugs []string, fileName string) string {
	for _, base := range searchPaths {
		var bestPath string
		var bestTime int64
		for _, slug := range slugs {
			path := filepath.Join(base, slug, fileName)
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			mt := info.ModTime().UnixNano()
			if mt > bestTime {
				bestTime = mt
				bestPath = path
			}
		}
		if bestPath != "" {
			return bestPath
		}
	}
	return ""
}

func findClaudeLatestSessionFile(searchPaths []string, workDir string) string {
	if workDir == "" {
		return ""
	}
	return findClaudeLatestSessionFileForCandidates(searchPaths, claudeProjectSlugCandidates(workDir))
}

func findClaudeLatestSessionFileForCandidates(searchPaths, slugs []string) string {
	for _, base := range searchPaths {
		var bestPath string
		var bestTime int64
		for _, slug := range slugs {
			path := filepath.Join(base, slug, "latest-session.jsonl")
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			mt := info.ModTime().UnixNano()
			if mt > bestTime {
				bestTime = mt
				bestPath = path
			}
		}
		if bestPath != "" {
			return bestPath
		}
	}
	return ""
}

func safeSessionLogFileName(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	return filepath.Base(sessionID) + ".jsonl"
}

// findSlugSessionFile searches slug-organized search paths for the most
// recently modified JSONL session file across all matching Claude
// project slug candidates. Files are stored at
// {searchPath}/{slug}/{sessionID}.jsonl where slug is the working
// directory path with "/" and "." replaced by "-".
func findSlugSessionFile(searchPaths []string, workDir string) string {
	return findSlugSessionFileForCandidates(searchPaths, claudeProjectSlugCandidates(workDir))
}

func findSlugSessionFileForCandidates(searchPaths, slugs []string) string {
	var globalBestPath string
	var globalBestTime int64
	for _, slug := range slugs {
		for _, base := range searchPaths {
			dir := filepath.Join(base, slug)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				mt := info.ModTime().UnixNano()
				if mt > globalBestTime {
					globalBestTime = mt
					globalBestPath = filepath.Join(dir, e.Name())
				}
			}
		}
	}
	return globalBestPath
}

// FindCodexSessionFile searches Codex's date-organized session directory
// (~/.codex/sessions/YYYY/MM/DD/*.jsonl) for the most recently modified
// session file whose embedded cwd matches workDir. Also searches
// symlinked session directories (e.g., aimux-managed accounts).
// Returns "" if no match is found or Codex sessions don't exist.
func FindCodexSessionFile(searchPaths []string, workDir string) string {
	if workDir == "" {
		return ""
	}
	var bestPath string
	var bestTime int64
	for _, root := range mergeCodexSearchPaths(searchPaths) {
		path := findCodexSessionFileIn(root, workDir)
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if mt := info.ModTime().UnixNano(); mt > bestTime {
			bestTime = mt
			bestPath = path
		}
	}
	return bestPath
}

// FindCodexSessionFileNear resolves a codex rollout transcript with a
// strictly bounded lookup: it opens only the local-date day directories
// (YYYY/MM/DD) intersecting [anchor-1m, anchor+window] under each merged
// codex root (plus symlinked extra roots, one level), filters files by the
// rollout filename timestamp ("rollout-2006-01-02T15-04-05-<uuid>.jsonl",
// written in local time by the codex CLI) falling inside that window with a
// 1-hour DST-fold tolerance on both sides, and confirms candidates via the
// session_meta cwd on the first line. Exactly one physically distinct cwd
// match is returned; multiple matches are refused as ambiguous (mirroring
// Manager.TranscriptPath's same-workdir guard) and zero matches, a zero
// anchor, or a non-positive window return "". Unlike
// FindCodexSessionFile it never walks the full date tree, so it is safe to
// call inside a prompt operation.
//
// "Local time" means the codex CLI's local time at write: filenames are
// parsed in THIS process's time.Local, so when the codex process ran under
// a different timezone than gc (containerized agents, or a host TZ change
// between write and read) the parsed timestamp shifts by the offset and an
// otherwise-valid rollout falls outside the window — discovery then returns
// "" and telemetry silently records nothing, consistent with the bounded
// best-effort contract.
func FindCodexSessionFileNear(searchPaths []string, workDir string, anchor time.Time, window time.Duration) string {
	if workDir == "" || anchor.IsZero() || window <= 0 {
		return ""
	}
	start := anchor.Add(-time.Minute)
	end := anchor.Add(window)
	var matches []string
	seen := make(map[string]bool)
	for _, root := range mergeCodexSearchPaths(searchPaths) {
		collectCodexRolloutsNear(root, workDir, start, end, true, seen, &matches)
		if len(matches) > 1 {
			return ""
		}
	}
	if len(matches) != 1 {
		return ""
	}
	return matches[0]
}

// appendCodexRolloutMatch appends path to matches unless its physical
// identity was already seen. One rollout is commonly reachable through two
// lexical paths (a configured root holding the aimux symlink AND the
// symlink's target listed directly); without physical dedup that single file
// would trip the ambiguity refusal. EvalSymlinks is used for identity
// comparison only — the FIRST lexical path is kept so the paired extractor's
// lexical containment validation still passes. On resolve error the lexical
// path itself is the identity.
func appendCodexRolloutMatch(path string, seen map[string]bool, matches *[]string) {
	key := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		key = resolved
	}
	if seen[key] {
		return
	}
	seen[key] = true
	*matches = append(*matches, path)
}

// collectCodexRolloutsNear appends in-window cwd-matching rollouts under one
// codex root to matches, deduplicated by physical identity via
// appendCodexRolloutMatch (seen is shared across roots by the caller).
// followExtraRoots permits one level of recursion into symlinked non-date
// roots (aimux-managed accounts), matching findCodexSessionFileIn's
// treatment of them. Recursion keeps the symlink-LEXICAL path
// (root/<link>/...) rather than the resolved target: the paired extractor
// (ExtractCodexTailUsageFromSearchPaths) validates containment lexically
// against the merged search roots, so a resolved path outside them would be
// discovered here only to be rejected there.
//
// The filename-timestamp filter accepts a 1-hour tolerance on both sides of
// [start, end]: during the autumn DST fold the wall time embedded in the
// filename is ambiguous and time.ParseInLocation can resolve it 1h off, so a
// genuinely in-window rollout would otherwise fail the exact filter. The cwd
// confirmation still guards attribution. The day-dir iteration is likewise
// padded by one calendar day on each side — the tolerance can cross local
// midnight, and startOfLocalDay in zones whose DST transition falls AT
// midnight (e.g. America/Santiago) can land on 23:00 of the previous day and
// skip the final calendar day; ENOENT readdirs are free.
func collectCodexRolloutsNear(root, workDir string, start, end time.Time, followExtraRoots bool, seen map[string]bool, matches *[]string) {
	tolStart := start.Add(-time.Hour)
	tolEnd := end.Add(time.Hour)
	firstDay := startOfLocalDay(start.In(time.Local)).AddDate(0, 0, -1)
	lastDay := startOfLocalDay(end.In(time.Local)).AddDate(0, 0, 1)
	for day := firstDay; !day.After(lastDay); day = day.AddDate(0, 0, 1) {
		dayDir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
		entries, err := os.ReadDir(dayDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ts, ok := codexRolloutFilenameTime(e.Name())
			if !ok || ts.Before(tolStart) || ts.After(tolEnd) {
				continue
			}
			path := filepath.Join(dayDir, e.Name())
			if codexSessionCWD(path) == workDir {
				appendCodexRolloutMatch(path, seen, matches)
				if len(*matches) > 1 {
					return
				}
			}
		}
	}
	if !followExtraRoots {
		return
	}
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range rootEntries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		name := e.Name()
		if len(name) == 4 && name >= "2000" && name <= "2099" {
			continue
		}
		// os.ReadDir follows the symlink on its own; non-directory or
		// dangling links simply fail every ReadDir in the recursion.
		collectCodexRolloutsNear(filepath.Join(root, name), workDir, start, end, false, seen, matches)
		if len(*matches) > 1 {
			return
		}
	}
}

// codexByIDDayDirCap bounds the day directories scanned per root by
// FindCodexSessionFileByID (~ one year plus padding). Ranges larger than the
// cap scan only the most recent cap days, iterating backward from the
// newest, so a pathologically old notBefore cannot turn the keyed lookup
// into an unbounded readdir sweep.
const codexByIDDayDirCap = 370

// FindCodexSessionFileByID resolves a codex rollout transcript by session
// identity: the codex CLI names rollouts
// "rollout-<localtime>-<session-uuid>.jsonl" where the uuid suffix equals
// the session_meta payload.id, so a captured session id keys the file
// directly — including resumed sessions, which APPEND to the original
// rollout whose filename timestamp predates any later wake. Local-day dirs
// from one day before notBefore through one day after notAfter are enumerated
// newest-first and capped at codexByIDDayDirCap per root. For a UUIDv7 session
// ID, the encoded creation day plus or minus two local calendar days is also
// eligible so adopted sessions whose rollout predates their bead remain
// attributable. Candidates match by filename, deduplicate by physical
// identity, refuse ambiguity, and require an exact session_meta cwd match.
func FindCodexSessionFileByID(searchPaths []string, workDir, sessionID string, notBefore, notAfter time.Time) string {
	const key = "session"
	found := FindCodexSessionFilesByID(searchPaths, []CodexSessionTarget{{
		Key:       key,
		WorkDir:   workDir,
		SessionID: sessionID,
		NotBefore: notBefore,
		NotAfter:  notAfter,
	}})
	return found[key]
}

// codexRolloutFilenameTime parses the local-time timestamp embedded in a
// codex rollout filename ("rollout-2006-01-02T15-04-05-<uuid>.jsonl").
func codexRolloutFilenameTime(name string) (time.Time, bool) {
	const prefix = "rollout-"
	const layout = "2006-01-02T15-04-05"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
		return time.Time{}, false
	}
	rest := name[len(prefix):]
	if len(rest) < len(layout) {
		return time.Time{}, false
	}
	ts, err := time.ParseInLocation(layout, rest[:len(layout)], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// startOfLocalDay truncates t to local midnight.
func startOfLocalDay(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.Local)
}

// CodexSessionCandidate is a Codex transcript whose session metadata matches a
// requested workdir.
type CodexSessionCandidate struct {
	Path      string
	WorkDir   string
	StartedAt time.Time
	ModTime   time.Time
}

// FindCodexSessionFileByIDNoWindow resolves a Codex transcript by provider
// session ID without a creation/wake window. Codex names rollouts
// "rollout-<localtime>-<session-uuid>.jsonl", so the id keys the file by its
// exact filename suffix. The scan walks date directories newest-first and
// returns the first "rollout-*-<sessionID>.jsonl" whose session_meta cwd equals
// workDir; only that matched transcript is opened, so cost scales with matches
// rather than with total Codex history. A session id containing path separators
// or ".." is rejected. Callers that have a creation/wake window should prefer
// FindCodexSessionFileByID, which bounds the scan by date and refuses ambiguous
// matches.
func FindCodexSessionFileByIDNoWindow(searchPaths []string, workDir, sessionID string) string {
	workDir = strings.TrimSpace(workDir)
	sessionID = strings.TrimSpace(sessionID)
	if workDir == "" || sessionID == "" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	suffix := "-" + sessionID + ".jsonl"
	seen := make(map[string]bool)
	for _, root := range mergeCodexSearchPaths(searchPaths) {
		if path := findCodexRolloutBySuffixIn(root, workDir, suffix, seen); path != "" {
			return path
		}
	}
	return ""
}

// findCodexRolloutBySuffixIn walks a Codex sessions directory newest-first and
// returns the first "rollout-*<suffix>" transcript whose session_meta cwd
// matches workDir. It recurses into symlinked non-date roots (aimux account
// roots) like findCodexSessionFileIn, guarding against symlink cycles via seen.
func findCodexRolloutBySuffixIn(sessDir, workDir, suffix string, seen map[string]bool) string {
	cleaned := filepath.Clean(sessDir)
	if seen[cleaned] {
		return ""
	}
	seen[cleaned] = true
	yearDirs, extraRoots := splitCodexSessionRoots(cleaned)
	sort.Sort(sort.Reverse(sort.StringSlice(yearDirs)))
	for _, year := range yearDirs {
		yearDir := filepath.Join(cleaned, year)
		for _, month := range listDirsReverse(yearDir) {
			monthDir := filepath.Join(yearDir, month)
			for _, day := range listDirsReverse(monthDir) {
				if path := findCodexRolloutBySuffixInDir(filepath.Join(monthDir, day), workDir, suffix); path != "" {
					return path
				}
			}
		}
	}
	for _, root := range extraRoots {
		resolved, err := filepath.EvalSymlinks(filepath.Join(cleaned, root))
		if err != nil {
			continue
		}
		if path := findCodexRolloutBySuffixIn(resolved, workDir, suffix, seen); path != "" {
			return path
		}
	}
	return ""
}

// findCodexRolloutBySuffixInDir returns the first rollout in dir whose name
// carries suffix and whose session_meta cwd matches workDir.
func findCodexRolloutBySuffixInDir(dir, workDir, suffix string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, suffix) {
			continue
		}
		path := filepath.Join(dir, name)
		if codexSessionCWD(path) == workDir {
			return path
		}
	}
	return ""
}

// FindCodexSessionFileInTimeWindow resolves a Codex transcript whose metadata
// start time uniquely falls inside [start, end). A zero end leaves the window
// open-ended. If the window matches zero or multiple transcripts, it returns
// empty to preserve same-workdir ambiguity guards. The disk scan is bounded to
// the date directories overlapping the window (padded one day for local-time
// skew) so cost scales with the window, not with total Codex history, and
// physical duplicates reachable through more than one merged root are counted
// once.
func FindCodexSessionFileInTimeWindow(searchPaths []string, workDir string, start, end time.Time) string {
	if start.IsZero() {
		return ""
	}
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return ""
	}
	firstDay := startOfLocalDay(start.In(time.Local)).AddDate(0, 0, -1)
	lastDay := startOfLocalDay(start.In(time.Local)).AddDate(0, 0, 1)
	if !end.IsZero() {
		lastDay = startOfLocalDay(end.In(time.Local)).AddDate(0, 0, 1)
	}
	if lastDay.Before(firstDay) {
		return ""
	}
	var candidates []CodexSessionCandidate
	seen := make(map[string]bool)
	for _, root := range mergeCodexSearchPaths(searchPaths) {
		collectCodexCandidatesInDays(root, workDir, firstDay, lastDay, true, seen, &candidates)
	}
	windowStart := start.Add(-2 * time.Second)
	match := ""
	for _, candidate := range candidates {
		candidateTime := codexCandidateSortTime(candidate)
		if candidateTime.IsZero() || candidateTime.Before(windowStart) {
			continue
		}
		if !end.IsZero() && !candidateTime.Before(end) {
			continue
		}
		if match != "" {
			return ""
		}
		match = candidate.Path
	}
	return match
}

// collectCodexCandidatesInDays appends Codex candidates matching workDir whose
// date directory falls within [firstDay, lastDay], newest day first and capped
// at codexByIDDayDirCap so an oversized range cannot become an unbounded sweep.
// Physical duplicates (symlink aliases across merged roots) are dropped via
// seen. followExtraRoots permits one level of recursion into symlinked non-date
// roots, mirroring collectCodexRolloutsByID.
func collectCodexCandidatesInDays(root, workDir string, firstDay, lastDay time.Time, followExtraRoots bool, seen map[string]bool, out *[]CodexSessionCandidate) {
	scanned := 0
	for day := lastDay; !day.Before(firstDay) && scanned < codexByIDDayDirCap; day = day.AddDate(0, 0, -1) {
		scanned++
		dayDir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
		appendCodexCandidatesFromDir(dayDir, workDir, seen, out)
	}
	if !followExtraRoots {
		return
	}
	_, extraRoots := splitCodexSessionRoots(root)
	for _, name := range extraRoots {
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if err != nil {
			continue
		}
		collectCodexCandidatesInDays(resolved, workDir, firstDay, lastDay, false, seen, out)
	}
}

// appendCodexCandidatesFromDir appends every Codex transcript in dir whose
// session_meta cwd matches workDir, deduplicated by physical file identity via
// seen so a rollout reachable through more than one root is counted once.
func appendCodexCandidatesFromDir(dir, workDir string, seen map[string]bool, out *[]CodexSessionCandidate) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		key := path
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			key = resolved
		}
		if seen[key] {
			continue
		}
		candidate, ok := codexSessionCandidate(path)
		if !ok || candidate.WorkDir != workDir {
			continue
		}
		seen[key] = true
		*out = append(*out, candidate)
	}
}

func codexCandidateSortTime(candidate CodexSessionCandidate) time.Time {
	if !candidate.StartedAt.IsZero() {
		return candidate.StartedAt
	}
	return candidate.ModTime
}

// splitCodexSessionRoots reads a Codex sessions directory and separates
// four-digit year directories (the YYYY/MM/DD date tree) from symlinked
// non-date roots (aimux-managed account roots). Entries that are neither a
// directory nor a symlink are ignored, and a read error yields empty slices.
func splitCodexSessionRoots(dir string) (yearDirs, extraRoots []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	for _, e := range entries {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		name := e.Name()
		if len(name) == 4 && name >= "2000" && name <= "2099" {
			yearDirs = append(yearDirs, name)
		} else if e.Type()&os.ModeSymlink != 0 {
			extraRoots = append(extraRoots, name)
		}
	}
	return yearDirs, extraRoots
}

// findCodexSessionFileIn searches a Codex sessions directory for the most
// recent session matching workDir. Scans date directories in reverse
// chronological order for efficiency. Also recurses into symlinked
// subdirectories that aren't date components (e.g., aimux session roots).
func findCodexSessionFileIn(sessDir, workDir string) string {
	yearDirs, extraRoots := splitCodexSessionRoots(sessDir)

	// Scan year dirs in reverse chronological order.
	sort.Sort(sort.Reverse(sort.StringSlice(yearDirs)))
	if path := scanYearDirs(sessDir, yearDirs, workDir); path != "" {
		return path
	}

	// Scan symlinked session roots (aimux-managed accounts).
	for _, root := range extraRoots {
		resolved, err := filepath.EvalSymlinks(filepath.Join(sessDir, root))
		if err != nil {
			continue
		}
		if path := findCodexSessionFileIn(resolved, workDir); path != "" {
			return path
		}
	}
	return ""
}

// scanYearDirs scans YYYY/MM/DD date tree for matching Codex sessions.
func scanYearDirs(base string, years []string, workDir string) string {
	for _, year := range years {
		yearDir := filepath.Join(base, year)
		months := listDirsReverse(yearDir)
		for _, month := range months {
			monthDir := filepath.Join(yearDir, month)
			days := listDirsReverse(monthDir)
			for _, day := range days {
				dayDir := filepath.Join(monthDir, day)
				if path := findCodexSessionInDir(dayDir, workDir); path != "" {
					return path
				}
			}
		}
	}
	return ""
}

// findCodexSessionInDir searches a single day directory for the most
// recently modified Codex session file matching workDir.
func findCodexSessionInDir(dir, workDir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	// Sort by mod time descending so we check newest first.
	type fileInfo struct {
		path    string
		modTime int64
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			path:    filepath.Join(dir, e.Name()),
			modTime: info.ModTime().UnixNano(),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime > files[j].modTime
	})

	for _, f := range files {
		if codexSessionCWD(f.path) == workDir {
			return f.path
		}
	}
	return ""
}

// codexSessionCWD reads the first line of a Codex JSONL session file and
// extracts the cwd from the session_meta payload. Returns "" if the file
// can't be read or doesn't contain a session_meta entry.
func codexSessionCWD(path string) string {
	candidate, ok := codexSessionCandidate(path)
	if !ok {
		return ""
	}
	return candidate.WorkDir
}

func codexSessionCandidate(path string) (CodexSessionCandidate, bool) {
	f, err := os.Open(path)
	if err != nil {
		return CodexSessionCandidate{}, false
	}
	defer f.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return CodexSessionCandidate{}, false
	}
	var meta struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			CWD       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &meta); err != nil {
		return CodexSessionCandidate{}, false
	}
	if meta.Type != "session_meta" {
		return CodexSessionCandidate{}, false
	}
	info, _ := os.Stat(path)
	var modTime time.Time
	if info != nil {
		modTime = info.ModTime()
	}
	startedAt := parseCodexSessionTime(meta.Payload.Timestamp)
	if startedAt.IsZero() {
		startedAt = parseCodexSessionTime(meta.Timestamp)
	}
	return CodexSessionCandidate{
		Path:      path,
		WorkDir:   meta.Payload.CWD,
		StartedAt: startedAt,
		ModTime:   modTime,
	}, true
}

func parseCodexSessionTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}

// listDirsReverse returns directory names sorted in reverse lexicographic
// order (newest date components first for YYYY/MM/DD trees).
func listDirsReverse(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// DefaultSearchPaths returns the default search paths for JSONL
// session files (~/.claude/projects/).
func DefaultSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".claude", "projects")}
}

// DefaultCodexSearchPaths returns the default search paths for Codex JSONL
// session files (~/.codex/sessions).
func DefaultCodexSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".codex", "sessions")}
}

// DefaultGeminiSearchPaths returns the default search paths for Gemini session
// files (~/.gemini/tmp).
func DefaultGeminiSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".gemini", "tmp")}
}

// DefaultKimiSearchPaths returns the default search paths for Kimi Code
// session files (~/.kimi/sessions).
func DefaultKimiSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".kimi", "sessions")}
}

// DefaultAntigravitySearchPaths returns the default search paths for Antigravity JSONL
// session files (~/.gemini/antigravity-cli/brain).
func DefaultAntigravitySearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".gemini", "antigravity-cli", "brain")}
}

// MergeSearchPaths merges default paths with user-configured extra paths,
// expanding ~ and deduplicating.
func MergeSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultSearchPaths(), extraPaths)
}

func mergeCodexSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultCodexSearchPaths(), extraPaths)
}

func mergeGeminiSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultGeminiSearchPaths(), extraPaths)
}

func mergePiSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultPiSearchPaths(), extraPaths)
}

func mergeAntigravitySearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultAntigravitySearchPaths(), extraPaths)
}

func mergePaths(defaults, extras []string) []string {
	seen := make(map[string]bool)
	var result []string
	add := func(p string) {
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[2:])
			}
		}
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	for _, p := range defaults {
		add(p)
	}
	for _, p := range extras {
		add(p)
	}
	return result
}

// ProviderFamily returns the canonical transcript provider family for provider.
func ProviderFamily(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch {
	case strings.Contains(p, "codex"):
		return "codex"
	case strings.Contains(p, "gemini"):
		return "gemini"
	case strings.Contains(p, "kimi"):
		return "kimi"
	case strings.Contains(p, "mimocode"):
		return "mimocode"
	case strings.Contains(p, "opencode"):
		return "opencode"
	case strings.Contains(p, "antigravity"):
		return "antigravity"
	case p == "pi" || strings.HasPrefix(p, "pi/") || strings.HasSuffix(p, "/pi") || strings.HasSuffix(p, "-pi") || strings.Contains(p, "-pi/"):
		return "pi"
	default:
		return p
	}
}

func claudeProjectSlugCandidates(workDir string) []string {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil
	}

	seenPaths := make(map[string]bool)
	var paths []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		path = filepath.Clean(path)
		if seenPaths[path] {
			return
		}
		seenPaths[path] = true
		paths = append(paths, path)
	}

	add(workDir)
	if abs, err := filepath.Abs(workDir); err == nil {
		add(abs)
	}
	add(pathutil.NormalizePathForCompare(workDir))

	for _, path := range append([]string(nil), paths...) {
		addDarwinClaudePathAliases(path, add)
	}

	seenSlugs := make(map[string]bool)
	var slugs []string
	for _, path := range paths {
		slug := ProjectSlug(path)
		if seenSlugs[slug] {
			continue
		}
		seenSlugs[slug] = true
		slugs = append(slugs, slug)
	}
	return slugs
}

func addDarwinClaudePathAliases(path string, add func(string)) {
	if runtime.GOOS != "darwin" {
		return
	}

	switch {
	case path == "/tmp":
		add("/private/tmp")
	case strings.HasPrefix(path, "/tmp/"):
		add("/private/tmp/" + strings.TrimPrefix(path, "/tmp/"))
	case path == "/private/tmp":
		add("/tmp")
	case strings.HasPrefix(path, "/private/tmp/"):
		add("/tmp/" + strings.TrimPrefix(path, "/private/tmp/"))
	}

	switch {
	case path == "/var":
		add("/private/var")
	case strings.HasPrefix(path, "/var/"):
		add("/private/var/" + strings.TrimPrefix(path, "/var/"))
	case path == "/private/var":
		add("/var")
	case strings.HasPrefix(path, "/private/var/"):
		add("/var/" + strings.TrimPrefix(path, "/private/var/"))
	}
}

// ProjectSlug converts an absolute path to the project directory slug
// convention: all "/" and "." are replaced with "-".
func ProjectSlug(absPath string) string {
	s := strings.ReplaceAll(absPath, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}
