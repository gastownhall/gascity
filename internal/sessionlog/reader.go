package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
}

// PaginationInfo describes the pagination state of a session response.
type PaginationInfo struct {
	HasOlderMessages       bool   `json:"has_older_messages"`
	TotalMessageCount      int    `json:"total_message_count"`
	ReturnedMessageCount   int    `json:"returned_message_count"`
	TruncatedBeforeMessage string `json:"truncated_before_message,omitempty"`
	TotalCompactions       int    `json:"total_compactions"`
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
	entries, err := parseFile(path)
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
	entries, err := parseFile(path)
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
	entries, err := parseFile(path)
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
	}, nil
}

// ReadFileRawOlder loads older raw (unfiltered) messages before a cursor.
func ReadFileRawOlder(path string, tailCompactions int, beforeMessageID string) (*Session, error) {
	entries, err := parseFile(path)
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
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening session file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	var entries []*Entry
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
			continue // skip malformed lines
		}
		// Preserve the raw JSON for API pass-through.
		raw := make([]byte, len(line))
		copy(raw, line)
		e.Raw = raw
		entries = append(entries, &e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning session file: %w", err)
	}

	return entries, nil
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
	default:
		return FindSessionFile(searchPaths, workDir)
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
	slug := ProjectSlug(workDir)
	for _, base := range searchPaths {
		path := filepath.Join(base, slug, fileName)
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func findClaudeLatestSessionFile(searchPaths []string, workDir string) string {
	if workDir == "" {
		return ""
	}
	slug := ProjectSlug(workDir)
	for _, base := range searchPaths {
		path := filepath.Join(base, slug, "latest-session.jsonl")
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path
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
// recently modified JSONL session file. Files are stored at
// {searchPath}/{slug}/{sessionID}.jsonl where slug is the working
// directory path with "/" and "." replaced by "-".
func findSlugSessionFile(searchPaths []string, workDir string) string {
	slug := ProjectSlug(workDir)
	var globalBestPath string
	var globalBestTime int64
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

// ProjectSlug converts an absolute path to the project directory slug
// convention: all "/" and "." are replaced with "-".
func ProjectSlug(absPath string) string {
	s := strings.ReplaceAll(absPath, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}
