package sessionlog

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CodexSessionTarget describes one exact Codex rollout lookup in a batch.
// Key is an opaque caller-owned correlation key and must be non-empty and
// unique within the batch.
type CodexSessionTarget struct {
	Key       string
	WorkDir   string
	SessionID string
	NotBefore time.Time
	NotAfter  time.Time
}

// FindCodexSessionFilesByID resolves exact Codex rollout paths for a batch of
// caller-owned targets. Its per-target lookup contract matches
// FindCodexSessionFileByID: workdir and session ID are trimmed, a zero
// NotAfter or reversed range is refused, the newest 370 padded lifecycle days
// plus a UUIDv7 creation-day hint padded by two local calendar days are
// eligible under each merged root and one-level symlinked extra root, physical
// aliases are deduplicated, multiple distinct filename matches are refused,
// and the sole candidate's session_meta cwd must match.
//
// The returned map contains found targets only. Invalid, missing, ambiguous,
// cwd-mismatched, empty-key, and duplicate-key targets are absent. Shared
// root/day directories are read at most once for the whole batch, and each
// rollout entry is parsed once before exact session-ID lookup. The batch
// considers targets in caller order and admits only complete windows that fit
// one scalar-sized shared day union. A hard request-wide ReadDir budget fails
// the whole batch closed, as does any ReadDir error other than a missing path;
// filesystem uncertainty therefore omits telemetry instead of resolving from
// a partial ambiguity window.
func FindCodexSessionFilesByID(searchPaths []string, targets []CodexSessionTarget) map[string]string {
	return findCodexSessionFilesByIDWithReadDir(searchPaths, targets, os.ReadDir)
}

const (
	codexUUIDv7HintPaddingDays = 2
	codexUUIDv7HintDayCount    = 2*codexUUIDv7HintPaddingDays + 1
	// One request may plan no more distinct days than the largest possible
	// scalar lookup: 370 lifecycle days plus five disjoint UUIDv7 hint days.
	codexBatchRequestDayDirCap = codexByIDDayDirCap + codexUUIDv7HintDayCount
	// Ten full scalar-sized physical roots fit below this cap. It also bounds
	// configured-root and one-level symlink multiplication for every request.
	codexBatchRequestReadDirCap = 4096
)

type codexBatchTarget struct {
	key       string
	workDir   string
	sessionID string
	firstDay  time.Time
	lastDay   time.Time
	seen      map[string]bool
	matches   []string
}

type codexBatchDay struct {
	relPath            string
	year               string
	targetsBySessionID map[string][]int
	matchesBySessionID map[string]*codexBatchMatches
}

type codexBatchMatches struct {
	seen  map[string]bool
	paths []string
}

type codexBatchEntryLookup func(string, map[string][]int) (string, bool)

// findCodexSessionFilesByIDWithReadDir is the dependency-injected batch
// implementation. Keeping directory reads explicit lets tests prove the
// batching invariant without mutating process-global hooks.
func findCodexSessionFilesByIDWithReadDir(
	searchPaths []string,
	targets []CodexSessionTarget,
	readDir func(string) ([]os.DirEntry, error),
) map[string]string {
	return findCodexSessionFilesByIDWithReaders(searchPaths, targets, readDir, codexSessionCWD, lookupCodexBatchEntrySessionID)
}

func findCodexSessionFilesByIDWithReaders(
	searchPaths []string,
	targets []CodexSessionTarget,
	readDir func(string) ([]os.DirEntry, error),
	readSessionCWD func(string) string,
	lookupEntry codexBatchEntryLookup,
) map[string]string {
	if len(targets) == 0 || readDir == nil || readSessionCWD == nil || lookupEntry == nil {
		return make(map[string]string)
	}
	states := buildCodexBatchTargets(targets)
	if len(states) == 0 {
		return make(map[string]string)
	}
	days := planCodexBatchDays(states)

	scanner := &codexBatchScanner{
		readDir:     readDir,
		lookupEntry: lookupEntry,
		days:        days,
		seenRoots:   make(map[string]bool),
	}
	if !scanner.scan(searchPaths) {
		// Matches collected before exhaustion are not authoritative: an
		// unscanned root/day could contain a second physical candidate.
		return make(map[string]string)
	}
	collectCodexBatchMatches(states, days)
	return resolveCodexBatchResults(states, readSessionCWD)
}

// buildCodexBatchTargets validates and normalizes batch targets into per-target
// scan state, applying the same admission contract as the scalar
// FindCodexSessionFileByID: the correlation key must be non-empty and unique in
// the batch, workdir and session ID are trimmed and required, NotAfter must be
// set, and the padded day window must not be reversed.
func buildCodexBatchTargets(targets []CodexSessionTarget) []codexBatchTarget {
	keyCounts := make(map[string]int, len(targets))
	for _, target := range targets {
		if target.Key != "" {
			keyCounts[target.Key]++
		}
	}

	states := make([]codexBatchTarget, 0, len(targets))
	for _, target := range targets {
		workDir := strings.TrimSpace(target.WorkDir)
		sessionID := strings.TrimSpace(target.SessionID)
		if target.Key == "" || keyCounts[target.Key] != 1 || workDir == "" || sessionID == "" || target.NotAfter.IsZero() {
			continue
		}
		firstDay := startOfLocalDay(target.NotBefore.In(time.Local)).AddDate(0, 0, -1)
		lastDay := startOfLocalDay(target.NotAfter.In(time.Local)).AddDate(0, 0, 1)
		if lastDay.Before(firstDay) {
			continue
		}
		states = append(states, codexBatchTarget{
			key:       target.Key,
			workDir:   workDir,
			sessionID: sessionID,
			firstDay:  firstDay,
			lastDay:   lastDay,
			seen:      make(map[string]bool),
		})
	}
	return states
}

// codexBatchTargetDays returns the eligible rollout day directories for one
// target, keyed by "YYYY/MM/DD" relative path with the year as value. It admits
// the newest codexByIDDayDirCap lifecycle days in range plus a UUIDv7
// creation-day hint padded by codexUUIDv7HintPaddingDays local calendar days.
func codexBatchTargetDays(state *codexBatchTarget) map[string]string {
	targetDays := make(map[string]string)
	addTargetDay := func(day time.Time) {
		year := day.Format("2006")
		relPath := filepath.Join(year, day.Format("01"), day.Format("02"))
		targetDays[relPath] = year
	}
	scanned := 0
	for day := state.lastDay; !day.Before(state.firstDay) && scanned < codexByIDDayDirCap; day = day.AddDate(0, 0, -1) {
		scanned++
		addTargetDay(day)
	}
	if createdAt, ok := codexUUIDv7CreationTime(state.sessionID); ok {
		creationDay := startOfLocalDay(createdAt.In(time.Local))
		for offset := -codexUUIDv7HintPaddingDays; offset <= codexUUIDv7HintPaddingDays; offset++ {
			addTargetDay(creationDay.AddDate(0, 0, offset))
		}
	}
	return targetDays
}

// planCodexBatchDays merges every admitted target's eligible days into a shared
// set of day directories to scan at most once, ordered newest first.
func planCodexBatchDays(states []codexBatchTarget) []*codexBatchDay {
	daysByPath := make(map[string]*codexBatchDay)
	for targetIndex := range states {
		registerCodexBatchTargetDays(daysByPath, &states[targetIndex], targetIndex)
	}
	days := make([]*codexBatchDay, 0, len(daysByPath))
	for _, day := range daysByPath {
		days = append(days, day)
	}
	sort.Slice(days, func(i, j int) bool {
		return days[i].relPath > days[j].relPath
	})
	return days
}

// registerCodexBatchTargetDays records one target's eligible days into the
// shared plan. A target whose days would push the batch past
// codexBatchRequestDayDirCap is dropped whole rather than registered on a
// partial window: matching from only the overlapping subset would be unsafe
// because an unplanned eligible day could contain a second physical rollout
// with the same ID.
func registerCodexBatchTargetDays(daysByPath map[string]*codexBatchDay, state *codexBatchTarget, targetIndex int) {
	targetDays := codexBatchTargetDays(state)

	newDays := 0
	for relPath := range targetDays {
		if daysByPath[relPath] == nil {
			newDays++
		}
	}
	if len(daysByPath)+newDays > codexBatchRequestDayDirCap {
		return
	}
	for relPath := range targetDays {
		batchDay := daysByPath[relPath]
		if batchDay == nil {
			batchDay = &codexBatchDay{
				relPath:            relPath,
				year:               targetDays[relPath],
				targetsBySessionID: make(map[string][]int),
				matchesBySessionID: make(map[string]*codexBatchMatches),
			}
			daysByPath[relPath] = batchDay
		}
		batchDay.targetsBySessionID[state.sessionID] = append(batchDay.targetsBySessionID[state.sessionID], targetIndex)
	}
}

// codexBatchScanner reads the planned day directories under each search root at
// most once, recording exact filename matches per day. A ReadDir error other
// than a missing path, or crossing codexBatchRequestReadDirCap, aborts the whole
// batch: filesystem uncertainty must omit telemetry rather than resolve from a
// partially scanned ambiguity window.
type codexBatchScanner struct {
	readDir      func(string) ([]os.DirEntry, error)
	lookupEntry  codexBatchEntryLookup
	days         []*codexBatchDay
	seenRoots    map[string]bool
	readDirCalls int
	exhausted    bool
	failed       bool
}

// aborted reports whether the request-wide ReadDir budget was exhausted or a
// non-missing ReadDir error was seen. Either makes collected matches unsafe.
func (s *codexBatchScanner) aborted() bool {
	return s.exhausted || s.failed
}

func (s *codexBatchScanner) boundedReadDir(path string) ([]os.DirEntry, error) {
	if s.readDirCalls >= codexBatchRequestReadDirCap {
		s.exhausted = true
		return nil, os.ErrInvalid
	}
	s.readDirCalls++
	entries, err := s.readDir(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.failed = true
	}
	return entries, err
}

// scan walks every merged search root and its one-level symlinked extra roots.
// It returns true only when the scan completed within the ReadDir budget with
// no non-missing directory error, i.e. the collected matches are authoritative.
func (s *codexBatchScanner) scan(searchPaths []string) bool {
	for _, mergedRoot := range mergeCodexSearchPaths(searchPaths) {
		if s.aborted() {
			break
		}
		root := filepath.Clean(mergedRoot)
		if !markCodexBatchRoot(root, s.seenRoots) {
			continue
		}
		// Probe/enumerate the root first so a missing configured path costs
		// one bounded read rather than every planned day beneath it.
		rootEntries, err := s.boundedReadDir(root)
		if err != nil {
			continue
		}
		s.scanRoot(root, codexBatchRootYears(rootEntries))
		if s.aborted() {
			break
		}
		s.scanExtraRoots(root, rootEntries)
	}
	return !s.aborted()
}

// scanExtraRoots follows one level of non-year symlinks beneath root, scanning
// each newly seen physical extra root for the planned days.
func (s *codexBatchScanner) scanExtraRoots(root string, rootEntries []os.DirEntry) {
	for _, entry := range rootEntries {
		if s.aborted() {
			return
		}
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		name := entry.Name()
		if codexBatchYearName(name) {
			continue
		}
		extraRoot := filepath.Join(root, name)
		if !markCodexBatchRoot(extraRoot, s.seenRoots) {
			continue
		}
		extraEntries, err := s.boundedReadDir(extraRoot)
		if err != nil {
			continue
		}
		s.scanRoot(extraRoot, codexBatchRootYears(extraEntries))
	}
}

// scanRoot records matches for every planned day that exists under root.
func (s *codexBatchScanner) scanRoot(root string, years map[string]bool) {
	for _, day := range s.days {
		if s.aborted() {
			return
		}
		if !years[day.year] {
			continue
		}
		s.scanDay(root, day)
	}
}

// scanDay reads one day directory and records each entry that names a target.
func (s *codexBatchScanner) scanDay(root string, day *codexBatchDay) {
	dayDir := filepath.Join(root, day.relPath)
	entries, err := s.boundedReadDir(dayDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		s.recordDayEntry(dayDir, day, entry)
	}
}

// recordDayEntry appends a matching rollout file to its target's day bucket,
// stopping once a session already has more than one distinct physical path.
func (s *codexBatchScanner) recordDayEntry(dayDir string, day *codexBatchDay, entry os.DirEntry) {
	if entry.IsDir() {
		return
	}
	sessionID, ok := s.lookupEntry(entry.Name(), day.targetsBySessionID)
	if !ok {
		return
	}
	matches := day.matchesBySessionID[sessionID]
	if matches == nil {
		matches = &codexBatchMatches{seen: make(map[string]bool)}
		day.matchesBySessionID[sessionID] = matches
	}
	if len(matches.paths) > 1 {
		return
	}
	path := filepath.Join(dayDir, entry.Name())
	appendCodexRolloutMatch(path, matches.seen, &matches.paths)
}

// collectCodexBatchMatches folds each day's per-session matches back onto the
// originating targets, preserving the ambiguity refusal by stopping once a
// target accumulates a second distinct path.
func collectCodexBatchMatches(states []codexBatchTarget, days []*codexBatchDay) {
	for _, day := range days {
		for sessionID, matches := range day.matchesBySessionID {
			for _, targetIndex := range day.targetsBySessionID[sessionID] {
				addCodexBatchTargetMatches(&states[targetIndex], matches.paths)
			}
		}
	}
}

// addCodexBatchTargetMatches folds one day's matched paths onto a target,
// stopping at the ambiguity threshold of a second distinct physical path.
func addCodexBatchTargetMatches(state *codexBatchTarget, paths []string) {
	if len(state.matches) > 1 {
		return
	}
	for _, path := range paths {
		appendCodexRolloutMatch(path, state.seen, &state.matches)
		if len(state.matches) > 1 {
			return
		}
	}
}

// resolveCodexBatchResults returns the found path for every target with exactly
// one match whose rollout session_meta cwd equals the requested workdir. Each
// distinct path's cwd is read at most once.
func resolveCodexBatchResults(states []codexBatchTarget, readSessionCWD func(string) string) map[string]string {
	found := make(map[string]string)
	cwdByPath := make(map[string]string)
	for i := range states {
		state := &states[i]
		if len(state.matches) != 1 {
			continue
		}
		path := state.matches[0]
		cwd, ok := cwdByPath[path]
		if !ok {
			cwd = readSessionCWD(path)
			cwdByPath[path] = cwd
		}
		if cwd == state.workDir {
			found[state.key] = state.matches[0]
		}
	}
	return found
}

func lookupCodexBatchEntrySessionID(name string, targetsBySessionID map[string][]int) (string, bool) {
	sessionID, ok := codexRolloutFilenameSessionID(name)
	if !ok || len(targetsBySessionID[sessionID]) == 0 {
		return "", false
	}
	return sessionID, true
}

func codexRolloutFilenameSessionID(name string) (string, bool) {
	const (
		prefix          = "rollout-"
		timestampLayout = "2006-01-02T15-04-05"
		extension       = ".jsonl"
	)
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, extension) {
		return "", false
	}
	idStart := len(prefix) + len(timestampLayout)
	idEnd := len(name) - len(extension)
	if idStart >= idEnd || name[idStart] != '-' {
		return "", false
	}
	if _, err := time.ParseInLocation(timestampLayout, name[len(prefix):idStart], time.Local); err != nil {
		return "", false
	}
	idStart++
	if idStart >= idEnd {
		return "", false
	}
	return name[idStart:idEnd], true
}

func codexBatchYearName(name string) bool {
	if len(name) != 4 {
		return false
	}
	for i := range name {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

func codexBatchRootYears(entries []os.DirEntry) map[string]bool {
	years := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if codexBatchYearName(name) && (entry.IsDir() || entry.Type()&os.ModeSymlink != 0) {
			years[name] = true
		}
	}
	return years
}

// codexUUIDv7CreationTime decodes the 48-bit Unix millisecond timestamp from
// a canonical-layout UUIDv7 prefix. Other UUID versions and malformed
// timestamp prefixes do not authorize scans outside the lifecycle window.
func codexUUIDv7CreationTime(sessionID string) (time.Time, bool) {
	if len(sessionID) != 36 || sessionID[8] != '-' || sessionID[13] != '-' || sessionID[18] != '-' || sessionID[23] != '-' || sessionID[14] != '7' {
		return time.Time{}, false
	}
	timestampHex := sessionID[:8] + sessionID[9:13]
	millis, err := strconv.ParseUint(timestampHex, 16, 48)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(millis)), true
}

// markCodexBatchRoot records a root by resolved physical identity while
// retaining the first lexical path for scanning and returned-path safety.
func markCodexBatchRoot(root string, seen map[string]bool) bool {
	identity := filepath.Clean(root)
	if absolute, err := filepath.Abs(identity); err == nil {
		identity = absolute
	}
	if resolved, err := filepath.EvalSymlinks(identity); err == nil {
		identity = resolved
	}
	if seen[identity] {
		return false
	}
	seen[identity] = true
	return true
}
