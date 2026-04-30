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
		paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, "")
		sess.Messages = paginated
		sess.Pagination = info
	}

	return sess, nil
}

// ReadProviderFile reads a provider-specific transcript file.
func ReadProviderFile(provider, path string, tailCompactions int) (*Session, error) {
	switch providerFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
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
		paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, "")
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
	switch providerFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
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

	paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, beforeMessageID)

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

	paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, beforeMessageID)

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
// Codex sessions do not currently support message-ID pagination, so the full
// provider transcript is returned.
func ReadProviderFileOlder(provider, path string, tailCompactions int, beforeMessageID string) (*Session, error) {
	switch providerFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
	default:
		return ReadFileOlder(path, tailCompactions, beforeMessageID)
	}
}

// ReadProviderFileRawOlder reads an older page of a provider-specific raw
// transcript. Codex sessions do not currently support message-ID pagination, so
// the full provider transcript is returned.
func ReadProviderFileRawOlder(provider, path string, tailCompactions int, beforeMessageID string) (*Session, error) {
	switch providerFamily(provider) {
	case "codex":
		return ReadCodexFile(path, tailCompactions)
	case "gemini":
		return ReadGeminiFile(path, tailCompactions)
	default:
		return ReadFileRawOlder(path, tailCompactions, beforeMessageID)
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
func sliceAtCompactBoundaries(messages []*Entry, tailCompactions int, beforeMessageID string) ([]*Entry, *PaginationInfo) {
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

// FindSessionFile searches for the most recently modified JSONL session
// file matching the given working directory. It tries slug-based lookup
// (Claude) across all search paths, then falls back to CWD-based lookup
// (Codex). Returns "" if no match is found.
func FindSessionFile(searchPaths []string, workDir string) string {
	// Try slug-based lookup first (Claude: {searchPath}/{slug}/*.jsonl).
	if path := findSlugSessionFile(searchPaths, workDir); path != "" {
		return path
	}
	// Fall back to Codex CWD-based lookup.
	return FindCodexSessionFile(searchPaths, workDir)
}

// FindSessionFileForProvider resolves the best available transcript file for a
// specific provider.
func FindSessionFileForProvider(searchPaths []string, provider, workDir string) string {
	switch providerFamily(provider) {
	case "codex":
		return FindCodexSessionFile(searchPaths, workDir)
	case "gemini":
		return FindGeminiSessionFile(searchPaths, workDir)
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
	switch providerFamily(provider) {
	case "codex":
		return FindCodexSessionFile(searchPaths, workDir)
	case "gemini":
		return FindGeminiSessionFile(searchPaths, workDir)
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

// findCodexSessionFileIn searches a Codex sessions directory for the most
// recent session matching workDir. Scans date directories in reverse
// chronological order for efficiency. Also recurses into symlinked
// subdirectories that aren't date components (e.g., aimux session roots).
func findCodexSessionFileIn(sessDir, workDir string) string {
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return ""
	}

	// Separate date-tree roots (YYYY dirs) from symlinked session roots.
	var yearDirs []string
	var extraRoots []string
	for _, e := range entries {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		name := e.Name()
		if len(name) == 4 && name >= "2000" && name <= "2099" {
			yearDirs = append(yearDirs, name)
		} else if e.Type()&os.ModeSymlink != 0 {
			// Symlinked directory — treat as an additional session root.
			extraRoots = append(extraRoots, name)
		}
	}

	// Scan year dirs in reverse chronological order.
	sort.Sort(sort.Reverse(sort.StringSlice(yearDirs)))
	if path := scanYearDirs(sessDir, yearDirs, workDir); path != "" {
		return path
	}

	// Scan symlinked session roots (aimux-managed accounts).
	for _, root := range extraRoots {
		rootDir := filepath.Join(sessDir, root)
		// Resolve symlink to get the actual directory.
		resolved, err := filepath.EvalSymlinks(rootDir)
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
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return ""
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			CWD string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &meta); err != nil {
		return ""
	}
	if meta.Type != "session_meta" {
		return ""
	}
	return meta.Payload.CWD
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

func providerFamily(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch {
	case strings.Contains(p, "codex"):
		return "codex"
	case strings.Contains(p, "gemini"):
		return "gemini"
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
